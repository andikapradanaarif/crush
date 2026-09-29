package main

import "fmt"

// first returns the first item of items.
func first(items []string) string {
	return items[0]
}

func main() {
	fmt.Println("tool starting")
	fmt.Println(first(nil))
	fmt.Println("tool ok")
}
