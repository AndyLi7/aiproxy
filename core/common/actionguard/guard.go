// Package actionguard carries an optional server-owned authorization check to
// each outgoing action. It is not populated from provider/client request fields.
package actionguard

import "context"

type key struct{}

func With(ctx context.Context, check func(context.Context) error) context.Context {
	return context.WithValue(ctx, key{}, check)
}
func Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if check, ok := ctx.Value(key{}).(func(context.Context) error); ok {
		return check(ctx)
	}
	return nil
}
