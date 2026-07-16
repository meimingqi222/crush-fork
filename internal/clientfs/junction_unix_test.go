//go:build !windows

package clientfs

func createJunction(string, string) error { return nil }
