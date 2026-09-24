package authn

import (
	"context"

	"github.com/pkg/errors"
)

type contextKey string

const keyUser contextKey = "user"

// ContextUser returns the authenticated identity on ctx, or panics if none
// is attached. The panic is the right behaviour for handlers that already
// ran the authentication middleware: the absence of a user is a programming
// error and surfacing it loudly is preferable to silently producing an empty
// permission set.
//
// Hooks that may run before the authn middleware (or on a route that
// bypasses it) must use LookupContextUser instead, which returns a
// (user, ok) pair that mirrors context.Value's contract.
func ContextUser(ctx context.Context) *User {
	user, ok := ctx.Value(keyUser).(*User)
	if !ok {
		panic(errors.New("no user in context"))
	}

	return user
}

// LookupContextUser returns the authenticated identity on ctx and whether one
// was attached. It never panics, so it is safe for hooks whose position in
// the chain is not guaranteed — pre-authn routes, defensive callers, and
// any helper that shares context plumbing with the auth extractor.
func LookupContextUser(ctx context.Context) (*User, bool) {
	user, ok := ctx.Value(keyUser).(*User)
	return user, ok
}

// SetContextUser attaches an authenticated identity to the context. The
// authentication middleware calls it; it is exported so the middlewares reading
// that identity can be tested without standing up a full authenticator.
func SetContextUser(ctx context.Context, user *User) context.Context {
	return context.WithValue(ctx, keyUser, user)
}
