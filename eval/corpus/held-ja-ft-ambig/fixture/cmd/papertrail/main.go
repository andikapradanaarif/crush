package main

import (
	"fmt"

	"example.com/papertrail/internal/doc"
)

func main() {
	d := &doc.Doc{Title: "memo", Pages: []string{"a", "b"}}
	fmt.Println("pages:", d.PageCount())
}
