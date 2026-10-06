//go:build windows

package lockfile

import "golang.org/x/sys/windows"

func isLink(path string) (bool, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attributes, err := windows.GetFileAttributes(p)
	return attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, err
}
