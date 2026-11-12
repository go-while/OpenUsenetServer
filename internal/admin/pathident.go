package admin

import (
	"net/http"
	"strings"
)

// refuseEnablePeerWithBadIdentity answers 409 and reports true when this server's name cannot
// identify it to peers (O-08). The startup check runs once; without this, a peer enabled through
// the portal after a warning-only start would be fed articles with a placeholder in Path.
func (p *Portal) refuseEnablePeerWithBadIdentity(w http.ResponseWriter) bool {
	problems := p.cfg.PathIdentityProblems()
	if len(problems) == 0 {
		return false
	}
	msgs := make([]string, len(problems))
	for i, pr := range problems {
		msgs[i] = pr.String()
	}
	writeJSON(w, http.StatusConflict, map[string]string{"error": "cannot enable a peer: " + strings.Join(msgs, "; ") +
		". Set it to this server's fully qualified domain name and restart, then enable the peer"})
	return true
}
