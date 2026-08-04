package controller

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/web/locale"

	"github.com/gin-gonic/gin"
)

// Node routes are escalation class.
//
// A node applies configuration as root on another machine, and adding one takes
// SSH credentials. That puts these routes alongside the database export, the
// panel binary and a host reboot: a permission BIT must never be enough, only
// super admin. The suite below drives the real guard rather than asserting on
// the route table, because the guard is the only thing that actually enforces.
func runNodeGuard(t *testing.T, user *model.User, method, path string) (int, string) {
	t.Helper()
	// A denial resolves where to send the caller instead, and for a reseller that
	// reads their profile from the database -- so refusing correctly still needs
	// one. Without it the guard panics in GORM before it can refuse anything.
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("LOGIN_USER_ROW", user)
		c.Set("base_path", "/")
		c.Set("I18n", func(i18nType locale.I18nType, key string, keyParams ...string) string { return key })
		c.Next()
	})
	g := r.Group("")
	NewNodeController(g)

	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// Every node route refuses a non-super admin, including one holding every
// permission bit the panel defines. If a bit were enough, an admin granted
// "core settings" to restart Xray could add a node and own another machine.
func TestNodeRoutesAreSuperAdminOnly(t *testing.T) {
	everyBit := &model.User{
		Id: 2, Enable: true,
		Permissions: model.PermAccessInbounds | model.PermPanelSettings |
			model.PermCoreSettings | model.PermXraySettings | model.PermManageResellers |
			model.PermAccessOverview | model.PermOverviewManage,
	}
	reseller := &model.User{Id: 3, Enable: true, IsReseller: true}

	routes := []struct{ method, path string }{
		{http.MethodGet, "/nodes/list"},
		{http.MethodPost, "/nodes/add"},
		{http.MethodPost, "/nodes/del/1"},
		{http.MethodGet, "/nodes/status/1"},
		{http.MethodGet, "/nodes/logs/1/xray"},
		{http.MethodPost, "/nodes/provision/1"},
	}

	for _, who := range []struct {
		name string
		user *model.User
	}{
		{"admin with every permission bit", everyBit},
		{"reseller", reseller},
		{"logged out", nil},
	} {
		for _, rt := range routes {
			code, body := runNodeGuard(t, who.user, rt.method, rt.path)
			// The panel answers a denied XHR with 200 + success:false so the
			// message reaches the user (see deny() in permission.go), so the
			// status alone does not tell us whether it passed.
			if strings.Contains(body, `"success":true`) {
				t.Errorf("%s reached %s %s: %s", who.name, rt.method, rt.path, body)
			}
			if code == http.StatusOK && !strings.Contains(body, `"success":false`) {
				t.Errorf("%s got a 200 with no denial marker on %s %s: %s",
					who.name, rt.method, rt.path, body)
			}
		}
	}
}

// A super admin gets past the guard. Without this the test above would pass for
// a controller that refused everyone, including the people who need it.
func TestNodeRoutesAdmitSuperAdmin(t *testing.T) {
	super := &model.User{Id: 1, Enable: true, IsSuperAdmin: true}
	_, body := runNodeGuard(t, super, http.MethodGet, "/nodes/list")
	// It will fail for want of a database, which is fine: what matters is that
	// the refusal is not an authorization one.
	if strings.Contains(body, "forbidden") {
		t.Errorf("super admin was refused by the guard: %s", body)
	}
}
