package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/access"
	"github.com/Silo-Server/silo-server/internal/api/handlers"
	apimw "github.com/Silo-Server/silo-server/internal/api/middleware"
	"github.com/Silo-Server/silo-server/internal/auth"
	"github.com/Silo-Server/silo-server/internal/models"
)

type pluginTestSessions struct{ role string }

func (s pluginTestSessions) ActiveSession(_ context.Context, id string) (auth.SessionAuthority, bool, error) {
	if id != "session-1" {
		return auth.SessionAuthority{}, false, nil
	}
	return auth.SessionAuthority{Role: s.role, Purpose: models.SessionPurposeAccount}, true, nil
}

// pluginTestViewer accepts "kid" and accepts "locked" only with its PIN
// token, as the viewer gate does.
type pluginTestViewer struct{}

func (pluginTestViewer) Resolve(_ context.Context, in access.ResolveInput) (access.Scope, error) {
	switch {
	case in.ProfileID == "kid", in.ProfileID == "primary":
		return access.Scope{ProfileID: in.ProfileID, ProfileVerified: true}, nil
	case in.ProfileID == "locked" && (in.ProfileToken == "pin-proof" || in.SkipPINVerification):
		return access.Scope{ProfileID: in.ProfileID, ProfileVerified: true}, nil
	case in.ProfileID == "locked":
		return access.Scope{}, access.ErrProfileUnverified
	default:
		return access.Scope{}, access.ErrProfileNotFound
	}
}

// pluginTestActingAdmin allows admin powers only through "primary".
func pluginTestActingAdmin(_ context.Context, in apimw.ActingAdminRequest) (bool, error) {
	return in.ProfileID == "primary", nil
}

func TestPluginAccessResolver(t *testing.T) {
	jwt := auth.NewJWTService("plugin-access-test-secret-0123456789", time.Hour, time.Hour)
	mint := func(role string, profileID string, ttl time.Duration) string {
		token, err := jwt.GeneratePluginAccessToken(7, role, "session-1", profileID, ttl)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	for _, tc := range []struct {
		name        string
		accountRole string
		cookie      string
		bearer      string
		headers     map[string]string
		want        pluginAccess
		wantRenewal bool
	}{
		{
			name: "launch cookie keeps its verified profile", accountRole: models.RoleUser,
			cookie: mint(models.RoleUser, "kid", handlers.PluginLaunchTTL),
			want:   pluginAccess{authenticated: true, userID: 7, profileID: "kid"},
		},
		{
			name: "declared profile needs its PIN proof", accountRole: models.RoleUser,
			bearer:  mint(models.RoleUser, "", handlers.PluginLaunchTTL),
			headers: map[string]string{"X-Profile-Id": "locked"},
			want:    pluginAccess{},
		},
		{
			name: "declared profile with its PIN proof", accountRole: models.RoleUser,
			bearer:  mint(models.RoleUser, "", handlers.PluginLaunchTTL),
			headers: map[string]string{"X-Profile-Id": "locked", "X-Profile-Token": "pin-proof"},
			want:    pluginAccess{authenticated: true, userID: 7, profileID: "locked"},
		},
		{
			name: "unknown declared profile", accountRole: models.RoleUser,
			bearer:  mint(models.RoleUser, "", handlers.PluginLaunchTTL),
			headers: map[string]string{"X-Profile-Id": "someone-else"},
			want:    pluginAccess{},
		},
		{
			name: "admin role acting as a child profile is not admin", accountRole: models.RoleAdmin,
			cookie: mint(models.RoleAdmin, "kid", handlers.PluginLaunchTTL),
			want:   pluginAccess{authenticated: true, userID: 7, profileID: "kid"},
		},
		{
			name: "admin role acting as the primary profile is admin", accountRole: models.RoleAdmin,
			cookie: mint(models.RoleAdmin, "primary", handlers.PluginLaunchTTL),
			want:   pluginAccess{authenticated: true, admin: true, userID: 7, profileID: "primary"},
		},
		{
			name: "a demoted account loses admin", accountRole: models.RoleUser,
			cookie: mint(models.RoleAdmin, "primary", handlers.PluginLaunchTTL),
			want:   pluginAccess{authenticated: true, userID: 7, profileID: "primary"},
		},
		{
			name: "a launch cookie past half its life is renewed", accountRole: models.RoleUser,
			cookie:      mint(models.RoleUser, "kid", handlers.PluginLaunchTTL/2-time.Second),
			want:        pluginAccess{authenticated: true, userID: 7, profileID: "kid"},
			wantRenewal: true,
		},
		{
			name: "a launch cookie whose profile was deleted is not renewed", accountRole: models.RoleUser,
			cookie: mint(models.RoleUser, "deleted", handlers.PluginLaunchTTL/2-time.Second),
			want:   pluginAccess{authenticated: true, userID: 7, profileID: "deleted"},
		},
		{
			name: "a bearer token is never renewed", accountRole: models.RoleUser,
			bearer: mint(models.RoleUser, "kid", handlers.PluginLaunchTTL/2-time.Second),
			want:   pluginAccess{authenticated: true, userID: 7, profileID: "kid"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver := &pluginAccessResolver{
				jwt: jwt, sessions: pluginTestSessions{role: tc.accountRole},
				viewer: pluginTestViewer{}, actingAdmin: pluginTestActingAdmin,
			}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/1/settings", nil)
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: auth.PluginAccessCookieName, Value: tc.cookie})
			}
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			for key, value := range tc.headers {
				req.Header.Set(key, value)
			}
			got := resolver.resolve(req)
			if (got.renewedToken != "") != tc.wantRenewal {
				t.Fatalf("renewed = %v, want %v", got.renewedToken != "", tc.wantRenewal)
			}
			got.renewedToken = ""
			if got != tc.want {
				t.Fatalf("access = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A renewed launch token keeps the session and profile and gets a full
// lifetime, so the page stays signed in as the same viewer.
func TestPluginAccessRenewalKeepsTheViewer(t *testing.T) {
	jwt := auth.NewJWTService("plugin-access-test-secret-0123456789", time.Hour, time.Hour)
	old, err := jwt.GeneratePluginAccessToken(7, models.RoleUser, "session-1", "kid", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &pluginAccessResolver{jwt: jwt, sessions: pluginTestSessions{role: models.RoleUser}, viewer: pluginTestViewer{}}
	req := httptest.NewRequest(http.MethodGet, "/api/v2/plugin-content/plugins/1/settings", nil)
	req.AddCookie(&http.Cookie{Name: auth.PluginAccessCookieName, Value: old})
	cookie := renewedPluginCookie(req, resolver.resolve(req), "/api/v2/plugin-content")
	if cookie == nil || cookie.Path != "/api/v2/plugin-content" || !cookie.HttpOnly || cookie.MaxAge != int(handlers.PluginLaunchTTL.Seconds()) {
		t.Fatalf("renewed cookie = %+v", cookie)
	}
	claims, err := jwt.ValidateToken(cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	if claims.SessionID != "session-1" || claims.ProfileID != "kid" || claims.TokenType != auth.TokenTypePluginAccess ||
		time.Until(claims.ExpiresAt.Time) < handlers.PluginLaunchTTL-time.Minute {
		t.Fatalf("renewed claims = %+v", claims)
	}
}

type pluginTestAPIKeys map[string]*models.APIKey

func (k pluginTestAPIKeys) GetByKey(_ context.Context, key string) (*models.APIKey, error) {
	if apiKey, ok := k[key]; ok {
		return apiKey, nil
	}
	return nil, auth.ErrAPIKeyNotFound
}

type pluginTestUsers map[int]*models.User

func (u pluginTestUsers) GetByID(_ context.Context, id int) (*models.User, error) {
	if user, ok := u[id]; ok {
		return user, nil
	}
	return nil, auth.ErrNotFound
}

// API keys carry no PIN proof, as on viewer routes, but a declared profile
// must still be the account's, and admin access still needs the primary
// profile.
func TestPluginAccessResolverAPIKeys(t *testing.T) {
	resolver := &pluginAccessResolver{
		jwt:      auth.NewJWTService("plugin-access-test-secret-0123456789", time.Hour, time.Hour),
		sessions: pluginTestSessions{role: models.RoleUser},
		apiKeys: pluginTestAPIKeys{
			"sa_user":   {ID: 1, UserID: 7},
			"sa_admin":  {ID: 2, UserID: 8},
			"sa_scoped": {ID: 3, UserID: 7, Scopes: []string{"webhooks"}},
			"sa_off":    {ID: 4, UserID: 9},
		},
		users: pluginTestUsers{
			7: {ID: 7, Role: models.RoleUser, Enabled: true},
			8: {ID: 8, Role: models.RoleAdmin, Enabled: true},
			9: {ID: 9, Role: models.RoleUser, Enabled: false},
		},
		viewer: pluginTestViewer{}, actingAdmin: pluginTestActingAdmin,
	}
	for _, tc := range []struct {
		name, key, profile string
		want               pluginAccess
	}{
		{"an unscoped key without a profile", "sa_user", "", pluginAccess{authenticated: true, userID: 7}},
		{"a PIN-locked profile needs no PIN proof", "sa_user", "locked", pluginAccess{authenticated: true, userID: 7, profileID: "locked"}},
		{"an unknown profile is refused", "sa_user", "someone-else", pluginAccess{}},
		{"a scoped key is refused", "sa_scoped", "", pluginAccess{}},
		{"a disabled account is refused", "sa_off", "", pluginAccess{}},
		{"an unknown key is refused", "sa_missing", "", pluginAccess{}},
		{"an admin key acting as a child profile is not admin", "sa_admin", "kid", pluginAccess{authenticated: true, userID: 8, profileID: "kid"}},
		{"an admin key acting as the primary profile is admin", "sa_admin", "primary", pluginAccess{authenticated: true, admin: true, userID: 8, profileID: "primary"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/1/settings", nil)
			req.Header.Set("Authorization", "Bearer "+tc.key)
			if tc.profile != "" {
				req.Header.Set("X-Profile-Id", tc.profile)
			}
			if got := resolver.resolve(req); got != tc.want {
				t.Fatalf("access = %+v, want %+v", got, tc.want)
			}
		})
	}
}
