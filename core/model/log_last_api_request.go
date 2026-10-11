package model

import (
	"context"
	"fmt"
	"time"
)

// lastAPIRequestExcludedSources are request sources that never count as the
// customer's own API use: the website playground and admin demos/trials.
// Rows with a NULL or empty source predate source tracking and do count.
//
// The filter is by request source only, never by key name: a key the customer
// named "playground" is an ordinary API key when its requests arrive as "api".
var lastAPIRequestExcludedSources = []string{
	RequestSourcePlayground,
	RequestSourceAdminDemo,
}

// GetGroupsLastAPIRequestAt returns, for each distinct group, the created_at of
// its newest log row whose request source is not the playground or an admin
// demo. Groups without such a row are absent from the map.
//
// It runs one indexed query per group (idx_group_creat, newest first, LIMIT 1)
// with ctx. When ctx ends before every group was looked up, it returns the
// groups that finished, the distinct groups that were not answered (in input
// order) and ctx.Err(). Any other query failure returns a nil map and the error.
func GetGroupsLastAPIRequestAt(
	ctx context.Context,
	groups []string,
) (found map[string]time.Time, unchecked []string, err error) {
	distinct := make([]string, 0, len(groups))

	seen := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		if group == "" {
			continue
		}

		if _, ok := seen[group]; ok {
			continue
		}

		seen[group] = struct{}{}
		distinct = append(distinct, group)
	}

	found = make(map[string]time.Time, len(distinct))

	for i, group := range distinct {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return found, distinct[i:], ctxErr
		}

		var createdAt []time.Time

		queryErr := LogDB.WithContext(ctx).
			Model(&Log{}).
			Where("group_id = ?", group).
			Where(
				"(request_source IS NULL OR request_source = '' OR request_source NOT IN ?)",
				lastAPIRequestExcludedSources,
			).
			Order("created_at DESC").
			Limit(1).
			Pluck("created_at", &createdAt).
			Error
		if queryErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return found, distinct[i:], ctxErr
			}

			return nil, nil, fmt.Errorf("query last api request of group %q: %w", group, queryErr)
		}

		if len(createdAt) > 0 && !createdAt[0].IsZero() {
			found[group] = createdAt[0]
		}
	}

	return found, nil, nil
}
