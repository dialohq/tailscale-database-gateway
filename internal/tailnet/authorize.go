package tailnet

import (
	"context"
	"fmt"
	"strings"

	"github.com/dialohq/tailscale-database-gateway/internal/credentials"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

type WhoIsClient interface {
	WhoIs(context.Context, string) (*apitype.WhoIsResponse, error)
	WhoIsForService(context.Context, string, tailcfg.ServiceName) (*apitype.WhoIsResponse, error)
}

type Authorizer struct {
	Client     WhoIsClient
	Service    tailcfg.ServiceName
	Capability tailcfg.PeerCapability
	Roles      credentials.Roles
}

type roleGrant struct {
	Role string `json:"role"`
}

func (a *Authorizer) Authorize(ctx context.Context, remote, role string) (credentials.Identity, credentials.Provider, error) {
	var who *apitype.WhoIsResponse
	var err error
	if a.Service == "" {
		who, err = a.Client.WhoIs(ctx, remote)
	} else {
		who, err = a.Client.WhoIsForService(ctx, remote, a.Service)
	}
	if err != nil {
		return credentials.Identity{}, nil, err
	}
	if who == nil || who.Node == nil {
		return credentials.Identity{}, nil, fmt.Errorf("peer has no Tailscale node identity")
	}
	identity := credentials.Identity{
		NodeName: who.Node.ComputedName,
		NodeID:   string(who.Node.StableID),
	}
	if who.Node.IsTagged() {
		if strings.TrimSpace(identity.NodeName) == "" {
			return credentials.Identity{}, nil, fmt.Errorf("tagged Tailscale node has no hostname")
		}
		identity.LoginName = identity.NodeName
	} else {
		if who.UserProfile == nil || who.UserProfile.LoginName == "" {
			return credentials.Identity{}, nil, fmt.Errorf("peer has no human Tailscale user profile")
		}
		identity.LoginName = who.UserProfile.LoginName
	}
	grants, err := tailcfg.UnmarshalCapJSON[roleGrant](who.CapMap, a.Capability)
	if err != nil {
		return credentials.Identity{}, nil, fmt.Errorf("decode Tailscale role grants: %w", err)
	}
	for _, grant := range grants {
		if strings.TrimSpace(grant.Role) != role {
			continue
		}
		if provider := a.Roles[role]; provider != nil {
			return identity, provider, nil
		}
	}
	return credentials.Identity{}, nil, fmt.Errorf("peer is not granted configured database role %q", role)
}
