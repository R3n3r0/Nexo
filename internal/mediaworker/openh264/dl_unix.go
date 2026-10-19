//go:build darwin || linux || freebsd

package openh264

import "github.com/ebitengine/purego"

func dlopen(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
}

func dlsym(h uintptr, name string) (uintptr, error) { return purego.Dlsym(h, name) }
