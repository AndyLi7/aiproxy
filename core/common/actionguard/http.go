package actionguard

import (
	"context"
	"errors"
	"net/http"
)

// DoHTTP keeps the client policy intact for unscoped calls. Scoped redirects
// are new remote actions: check the original owner after the existing redirect
// policy, immediately before net/http sends the next request. Clone the client
// rather than changing a shared CheckRedirect callback between concurrent users.
func DoHTTP(client *http.Client, req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if _, scoped := ctx.Value(key{}).(func(context.Context) error); !scoped {
		return client.Do(req)
	}
	guarded := *client
	guarded.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if client.CheckRedirect != nil {
			if err := client.CheckRedirect(next, via); err != nil {
				return err
			}
		} else if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return Check(ctx)
	}
	return guarded.Do(req)
}
