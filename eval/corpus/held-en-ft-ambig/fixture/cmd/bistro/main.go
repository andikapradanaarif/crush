package main

import (
	"fmt"

	"example.com/bistro/internal/menu"
)

func main() {
	m := &menu.Menu{}
	m.Add(menu.Dish{Name: "nasi goreng", Cents: 850})
	fmt.Println("dishes:", m.Len())
}
