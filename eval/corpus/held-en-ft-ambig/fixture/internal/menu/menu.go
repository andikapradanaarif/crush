// Package menu tracks the counter's dishes.
package menu

// Dish is a single menu entry priced in cents.
type Dish struct {
	Name  string
	Cents int
}

// Menu is the counter's dish list; names are unique.
type Menu struct {
	dishes []Dish
}

// Add registers a dish; a duplicate name is rejected and the
// menu is left unchanged.
func (m *Menu) Add(d Dish) bool {
	for _, e := range m.dishes {
		if e.Name == d.Name {
			return false
		}
	}
	m.dishes = append(m.dishes, d)
	return true
}

// Price looks up a dish's price in cents.
func (m *Menu) Price(name string) (int, bool) {
	for _, e := range m.dishes {
		if e.Name == name {
			return e.Cents, true
		}
	}
	return 0, false
}

// Len reports how many dishes are registered.
func (m *Menu) Len() int { return len(m.dishes) }
