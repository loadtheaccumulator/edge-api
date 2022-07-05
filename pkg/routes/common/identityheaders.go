package common

import (
	"context"
	"fmt"
	"net/http"

	"github.com/redhatinsights/edge-api/config"
	"github.com/redhatinsights/platform-go-middlewares/identity"
)

// GetDefaultIdentity is a function to create the default identity struct. Structs can not be const in go
func GetDefaultIdentity() identity.XRHID {
	DefaultIdentity := identity.XRHID{}
	DefaultIdentity.Identity.OrgID = "00000000"
	return DefaultIdentity
}

// GetIdentity from http request header
func GetIdentity(r *http.Request) (string, error) {
	return GetAccountFromContext(r.Context())
}

// GetIdentityFromContext determines identity from supplied context
func GetIdentityFromContext(ctx context.Context) (identity.XRHID, error) {
	if config.Get() != nil {
		if !config.Get().Auth {
			return GetDefaultIdentity(), nil
		}
		if ctx.Value(identity.Key) != nil {
			ident := identity.Get(ctx)
			return ident, nil
		}
	}
	return identity.XRHID{}, fmt.Errorf("cannot find account number")
}
