//go:build windows

package main

import "testing"

func TestSingleInstanceMutex(t *testing.T) {
	first, duplicate, err := acquireSingleInstance()
	if err != nil {
		t.Fatal(err)
	}
	if duplicate {
		t.Skip("WorkBuddy2API is already running")
	}
	defer first.Close()

	second, duplicate, err := acquireSingleInstance()
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate {
		if second != nil {
			_ = second.Close()
		}
		t.Fatal("second acquire did not report an existing instance")
	}
	if second != nil {
		t.Fatal("duplicate acquire returned a lock")
	}
}
