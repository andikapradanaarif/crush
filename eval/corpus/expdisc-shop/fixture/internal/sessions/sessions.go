// Package sessions is the in-memory token store for the CLI's
// single-user mode.
package sessions

var tokens = map[string]string{}

// Open issues a token for user.
func Open(user, token string) { tokens[token] = user }

// Who resolves a token.
func Who(token string) (string, bool) {
	u, ok := tokens[token]
	return u, ok
}
