package main

import (
	"fmt"
	"regexp"
)

func main() {
	pattern := "c\\.agents["
	r, err := regexp.Compile(pattern)
	if err != nil {
		fmt.Printf("Go regex error: %v\n", err)
	} else {
		fmt.Printf("OK: %v\n", r)
	}
}
