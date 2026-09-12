// Package billing implements the Fees API: creating bills for a fee period,
// accruing fee line items onto them, and closing them into a final invoice.
package billing

import "context"

// HealthResponse reports service liveness.
type HealthResponse struct {
	Status string `json:"status"`
}

// Health reports whether the service is up.
//
//encore:api public method=GET path=/health
func Health(ctx context.Context) (*HealthResponse, error) {
	return &HealthResponse{Status: "ok"}, nil
}
