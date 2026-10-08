package session

import "context"

type deleteAuditContextKey struct{}

type deleteAuditRequest struct {
	IP        string
	UserAgent string
}

// WithDeleteAuditRequest adds the HTTP request metadata used by the session
// manager's durable delete audit event.
func WithDeleteAuditRequest(ctx context.Context, ip, userAgent string) context.Context {
	return context.WithValue(ctx, deleteAuditContextKey{}, deleteAuditRequest{IP: ip, UserAgent: userAgent})
}

func deleteAuditRequestFromContext(ctx context.Context) deleteAuditRequest {
	if ctx == nil {
		return deleteAuditRequest{}
	}
	request, _ := ctx.Value(deleteAuditContextKey{}).(deleteAuditRequest)
	return request
}
