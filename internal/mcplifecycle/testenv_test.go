package mcplifecycle

import "os"

func init() {
	_ = os.Setenv("CRUSH_DISABLE_PROVIDER_AUTO_UPDATE", "1")
}
