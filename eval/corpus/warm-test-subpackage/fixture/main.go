package main

import (
	"fmt"

	"example.com/multipkg/pkg/calc"
)

func main() {
	fmt.Println(calc.Divide(10, 2))
}
