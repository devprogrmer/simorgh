package controller

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/mhsanaei/3x-ui/v2/web/session"
)

// Separating the reseller panel's URL from the admin panel's.
//
// The value is blast radius, not secrecy. A reseller's URL is handed to every
// person who sells for you and it spreads -- pasted into chats, saved in
// browsers, typed on borrowed machines. One shared path means every one of those
// people also knows where the panel that administers the whole fleet lives, so
// an attack on it needs no discovery step. Two paths keep the admin panel's
// location something a reseller never learns doing normal work.
//
// This is defence in depth and nothing more. The permission middleware is still
// the enforcement: every route stays reachable by direct request, and a reseller
// who guesses the admin path is refused by requirePerm exactly as before. A path
// nobody has published is not an access control, and treating it as one would be
// the actual security mistake.

// EnforcePathSeparation refuses a session that arrived on the wrong panel's path.
//
// It runs AFTER authentication, so it can see who the caller is. A reseller on
// the admin path and an admin on the reseller path are both redirected to their
// own, rather than being served a panel whose navigation is built for the other
// role.
func EnforcePathSeparation(adminBase, resellerBase string) gin.HandlerFunc {
	// Not configured means one shared path, which is the behaviour every existing
	// install already has. Returning a no-op keeps the middleware off the hot
	// path entirely rather than comparing two equal strings on every request.
	if resellerBase == "" || resellerBase == adminBase {
		return func(c *gin.Context) { c.Next() }
	}

	return func(c *gin.Context) {
		user := session.GetLoginUser(c)
		if user == nil {
			c.Next() // not logged in yet; the login handler decides
			return
		}

		path := c.Request.URL.Path
		onReseller := strings.HasPrefix(path, resellerBase)
		onAdmin := strings.HasPrefix(path, adminBase) && !onReseller

		switch {
		case user.IsReseller && onAdmin:
			// Deliberately a redirect to their own panel rather than a 404 or a
			// 403. A reseller landing here has almost always followed a stale
			// bookmark from before the paths were split, and sending them
			// somewhere useful costs the operator one fewer support message.
			redirectTo(c, resellerBase)
		case !user.IsReseller && onReseller:
			redirectTo(c, adminBase)
		default:
			c.Next()
		}
	}
}

func redirectTo(c *gin.Context, base string) {
	if isAjax(c) {
		// An XHR gets JSON, matching how deny() answers elsewhere: axios rejects
		// a redirect it cannot follow, and the user would see a transport error
		// instead of being moved.
		pureJsonMsg(c, http.StatusOK, false, "wrong panel for this account")
		c.Abort()
		return
	}
	c.Redirect(http.StatusTemporaryRedirect, base)
	c.Abort()
}
