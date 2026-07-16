package terminal

import (
	"os"
	"runtime"
	"strings"
)

func mergedEnvironment(overrides map[string]string) []string {
	environment := append([]string(nil), os.Environ()...)
	for key, value := range overrides {
		prefix := key + "="
		replaced := false
		for index, current := range environment {
			if envKeyEqual(strings.SplitN(current, "=", 2)[0], key) {
				environment[index] = prefix + value
				replaced = true
				break
			}
		}
		if !replaced {
			environment = append(environment, prefix+value)
		}
	}
	environment = append(environment, "CRUSH=1", "AGENT=crush", "AI_AGENT=crush")
	return environment
}

func envKeyEqual(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}
