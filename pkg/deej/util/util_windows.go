package util

import (
	"fmt"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/lxn/win"
	"github.com/mitchellh/go-ps"
)

const (
	getCurrentWindowInternalCooldown = time.Millisecond * 150
)

var (
	getCurrentWindowMutex      sync.Mutex
	lastGetCurrentWindowResult []string
	lastGetCurrentWindowCall   = time.Now()
)

type enumChildWindowsContext struct {
	ownerPID uint32
	results  []string
}

var (
	enumChildWindowsCallback = syscall.NewCallback(func(childHWND win.HWND, lParam uintptr) uintptr {
		ctx := (*enumChildWindowsContext)(unsafe.Pointer(lParam))
		if ctx == nil {
			return 1
		}

		var childPID uint32
		win.GetWindowThreadProcessId(childHWND, &childPID)

		if childPID != 0 && childPID != ctx.ownerPID {
			actualProcess, err := ps.FindProcess(int(childPID))
			if err == nil && actualProcess != nil {
				ctx.results = append(ctx.results, actualProcess.Executable())
			}
		}

		return 1
	})
)

func getCurrentWindowProcessNames() ([]string, error) {
	getCurrentWindowMutex.Lock()
	defer getCurrentWindowMutex.Unlock()

	// apply an internal cooldown on this function to avoid calling windows API functions too frequently.
	// return a cached value during that cooldown
	now := time.Now()
	if lastGetCurrentWindowCall.Add(getCurrentWindowInternalCooldown).After(now) {
		return lastGetCurrentWindowResult, nil
	}

	lastGetCurrentWindowCall = now

	// the logic of this implementation is a bit convoluted because of the way UWP apps
	// (also known as "modern win 10 apps" or "microsoft store apps") work.
	// these are rendered in a parent container by the name of ApplicationFrameHost.exe.
	// when windows's GetForegroundWindow is called, it returns the window owned by that parent process.
	// so whenever we get that, we need to go and look through its child windows until we find one with a different PID.
	// this behavior is most common with UWP, but it actually applies to any "container" process:
	// an acceptable approach is to return a slice of possible process names that could be the "right" one, looking
	// them up is fairly cheap and covers the most bases for apps that hide their audio-playing inside another process
	// (like steam, and the league client, and any UWP app)

	// get the current foreground window
	hwnd := win.GetForegroundWindow()
	var ownerPID uint32

	// get its PID and put it in our window info struct
	win.GetWindowThreadProcessId(hwnd, &ownerPID)

	// check for system PID (0)
	if ownerPID == 0 {
		return nil, nil
	}

	// find the process name corresponding to the parent PID
	process, err := ps.FindProcess(int(ownerPID))
	if err != nil {
		return nil, fmt.Errorf("get parent process for pid %d: %w", ownerPID, err)
	}

	ctx := &enumChildWindowsContext{
		ownerPID: ownerPID,
		results:  []string{process.Executable()},
	}

	// iterate its child windows, adding their names too using the static callback
	win.EnumChildWindows(hwnd, enumChildWindowsCallback, uintptr(unsafe.Pointer(ctx)))

	// cache & return whichever executable names we ended up with
	lastGetCurrentWindowResult = ctx.results
	return ctx.results, nil
}
