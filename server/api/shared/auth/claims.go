package auth

import "context"

type Role string

const (
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleViewer   Role = "viewer"
)

func (r Role) CanSendCommand() bool {
	return r == RoleAdmin || r == RoleOperator
}

func (r Role) IsAdmin() bool {
	return r == RoleAdmin
}

type Claims struct {
	Subject    string   `json:"sub"`
	Email      string   `json:"email"`
	OrgID      string   `json:"org_id"`
	Role       Role     `json:"role"`
	VehicleSet []string `json:"vehicle_set"`
}

// CanAccessVehicle returns true when the claims grant access to vehicleID.
// Admins always have access. An empty VehicleSet means unrestricted.
// A wildcard "*" entry grants access to every vehicle.
func (c *Claims) CanAccessVehicle(vehicleID string) bool {
	if c.Role.IsAdmin() || len(c.VehicleSet) == 0 {
		return true
	}
	for _, v := range c.VehicleSet {
		if v == "*" || v == vehicleID {
			return true
		}
	}
	return false
}

type ctxKey struct{}

func ContextWithClaims(ctx context.Context, c *Claims) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

func ClaimsFromContext(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(ctxKey{}).(*Claims)
	return c, ok
}
