package openh264

import "golang.org/x/sys/windows"

func dlopen(path string) (uintptr, error) {
	h, err := windows.LoadLibrary(path)
	return uintptr(h), err
}

func dlsym(h uintptr, name string) (uintptr, error) {
	return windows.GetProcAddress(windows.Handle(h), name)
}
