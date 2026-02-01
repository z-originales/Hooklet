package auth

import "context"

// Context key for admin bypass.
// This key is private to the package, ensuring only this package can create or check it.
type contextKey string

const ctxKeyAdminBypass contextKey = "admin_bypass"

// WithAdminBypass marks the context as trusted admin.
func WithAdminBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyAdminBypass, true)
}

// IsAdminBypass checks if the request comes from trusted admin context.
func IsAdminBypass(ctx context.Context) bool {
	bypass, ok := ctx.Value(ctxKeyAdminBypass).(bool)
	return ok && bypass
}
