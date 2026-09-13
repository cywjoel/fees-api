// Package billing implements the Fees API: creating bills for a fee period,
// accruing fee line items onto them, and closing them into a final invoice.
package billing

import "context"

type HealthResponse struct {
	Status string `json:"status"`
}

//encore:api public method=GET path=/health
func Health(ctx context.Context) (*HealthResponse, error) {
	return &HealthResponse{Status: "ok"}, nil
}
