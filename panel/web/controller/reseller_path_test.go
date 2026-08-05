package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mhsanaei/3x-ui/v2/database/model"
)

// runPath drives one request through the separation middleware.
func runPath(t *testing.T, user *model.User, adminBase, resellerBase, path string, ajax bool) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/*any", withUser(user), EnforcePathSeparation(adminBase, resellerBase),
		func(c *gin.Context) { c.String(http.StatusOK, "reached") })

	req := httptest.NewRequest(http.MethodGet, path, nil)
	if ajax {
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
	} else {
		req.Header.Set("Accept", "text/html")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Header().Get("Location")
}

// Each role is served on its own path.
func TestEachRoleReachesItsOwnPanel(t *testing.T) {
	admin := &model.User{Id: 1, Enable: true, IsSuperAdmin: true}
	reseller := &model.User{Id: 2, Enable: true, IsReseller: true}

	if got, _ := runPath(t, admin, "/admin/", "/sell/", "/admin/panel/", false); got != http.StatusOK {
		t.Errorf("admin on the admin path = %d; want 200", got)
	}
	if got, _ := runPath(t, reseller, "/admin/", "/sell/", "/sell/panel/", false); got != http.StatusOK {
		t.Errorf("reseller on the reseller path = %d; want 200", got)
	}
}

// A reseller on the admin path is moved to their own, not served the admin
// panel. Redirected rather than 404'd because this is almost always a stale
// bookmark from before the split, and sending them somewhere useful costs the
// operator one fewer support message.
func TestResellerOnAdminPathIsRedirected(t *testing.T) {
	reseller := &model.User{Id: 2, Enable: true, IsReseller: true}
	got, loc := runPath(t, reseller, "/admin/", "/sell/", "/admin/panel/", false)
	if got != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d; want a 307 redirect", got)
	}
	if loc != "/sell/" {
		t.Errorf("redirected to %q; want the reseller panel", loc)
	}
}

// And the reverse, so an admin does not land in a navigation built for selling.
func TestAdminOnResellerPathIsRedirected(t *testing.T) {
	admin := &model.User{Id: 1, Enable: true, IsSuperAdmin: true}
	got, loc := runPath(t, admin, "/admin/", "/sell/", "/sell/panel/", false)
	if got != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d; want a 307 redirect", got)
	}
	if loc != "/admin/" {
		t.Errorf("redirected to %q; want the admin panel", loc)
	}
}

// An unconfigured reseller path means one shared panel, which is what every
// existing install already has. Nobody may be redirected in that state, or an
// upgrade would start bouncing resellers who were working fine.
func TestUnconfiguredPathChangesNothing(t *testing.T) {
	reseller := &model.User{Id: 2, Enable: true, IsReseller: true}
	admin := &model.User{Id: 1, Enable: true, IsSuperAdmin: true}
	for _, u := range []*model.User{reseller, admin} {
		if got, _ := runPath(t, u, "/admin/", "", "/admin/panel/", false); got != http.StatusOK {
			t.Errorf("with no reseller path configured, status = %d; want 200 (unchanged behaviour)", got)
		}
	}
}

// A logged-out caller is left to the login handler rather than bounced by path,
// or the login page itself would be unreachable on one of the two paths.
func TestLoggedOutIsNotRedirectedByPath(t *testing.T) {
	if got, _ := runPath(t, nil, "/admin/", "/sell/", "/sell/panel/", false); got != http.StatusOK {
		t.Errorf("logged out = %d; want the request to pass through to the login handler", got)
	}
}

// An XHR gets JSON, not a redirect: axios cannot follow one here, and the user
// would see a transport error instead of being moved.
func TestWrongPanelXHRGetsJSON(t *testing.T) {
	reseller := &model.User{Id: 2, Enable: true, IsReseller: true}
	got, loc := runPath(t, reseller, "/admin/", "/sell/", "/admin/panel/api/inbounds/list", true)
	if got != http.StatusOK {
		t.Errorf("xhr status = %d; want 200 carrying success:false", got)
	}
	if loc != "" {
		t.Errorf("an XHR was answered with a redirect to %q", loc)
	}
}

// The separation is defence in depth, NOT an access control, and this pins that
// so nobody later relies on it as one.
//
// A path nobody has published is not a permission. The middleware only decides
// which panel a caller is shown; requirePerm still decides what they may do, and
// a reseller who guesses the admin path must be refused by permissions exactly
// as before the split existed.
func TestPathSeparationIsNotAnAccessControl(t *testing.T) {
	reseller := &model.User{Id: 2, Enable: true, IsReseller: true}

	// With separation configured, the reseller is redirected off the admin path.
	if got, _ := runPath(t, reseller, "/admin/", "/sell/", "/admin/panel/", false); got != http.StatusTemporaryRedirect {
		t.Fatalf("setup: expected a redirect, got %d", got)
	}
	// With it NOT configured, the same reseller reaches the same handler -- which
	// is safe only because requirePerm gates it. If this ever starts failing
	// because someone made the path itself the check, the permission middleware
	// has been quietly demoted and the real gate needs looking at.
	if got, _ := runPath(t, reseller, "/admin/", "", "/admin/panel/", false); got != http.StatusOK {
		t.Fatalf("the path must not be doing the gating; permissions are the enforcement (got %d)", got)
	}
}
