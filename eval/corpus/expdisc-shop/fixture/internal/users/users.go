// Package users is the customer record store.
package users

// User is a registered customer.
type User struct {
	ID    string
	Email string
	Zone  string
}

var byID = map[string]User{
	"u1": {ID: "u1", Email: "amy@shop.test", Zone: "standard"},
	"u2": {ID: "u2", Email: "ben@shop.test", Zone: "flat"},
}

// ByID resolves a user.
func ByID(id string) (User, bool) {
	u, ok := byID[id]
	return u, ok
}
