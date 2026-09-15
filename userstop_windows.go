package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A per-user install has no service manager entry for its supervisor. The
// running predecessor is a process of the installing account started from the
// install folder, and removal of that folder only closes supervisors that own
// a Restart Manager shutdown window (0.1.6 and later). An older supervisor keeps
// answering on the bus from its deleted image, so the incoming package stops it
// first. Termination loses nothing: leases lapse, records stay, and the next
// supervisor adopts the work (cmdStop).
//
// The heartbeat PID is never used here. A PID names whatever holds it now; the
// evidence is the owner SID, the image path and a retained handle whose creation
// time precedes the enumeration.

const userStopBudget = time.Minute

var predecessorImages = map[string]bool{"jobd.exe": true, "jobdw.exe": true, "openabstractions.exe": true}

// errNotCandidate marks a process that exited before it could be opened or that
// this account cannot open. Neither can be a verified predecessor of this account.
var errNotCandidate = errors.New("process is not openable by this account")

type processEntry struct {
	pid  uint32
	name string
}

// heldProcess is a retained handle. terminate must act on the same process
// object the handle names and refuse when its creation time differs.
type heldProcess interface {
	image() (string, error)
	owner() (string, error)
	created() (int64, error)
	terminate(created int64) error
	exited() (bool, error)
	close() error
}

type userStopOps struct {
	// snapshot lists processes and returns a time no earlier than the creation of
	// any process it lists.
	snapshot func() (int64, []processEntry, error)
	open     func(pid uint32) (heldProcess, error)
	// record, when set, receives every verified predecessor before any is
	// terminated. A failure terminates nothing.
	record func([]predecessor) error
}

type predecessor struct {
	pid     uint32
	image   string
	created int64
	process heldProcess
}

// serviceStopUser stops predecessors under the incoming folder and, when related
// ProductCodes are given, under each related product's recorded folder.
func serviceStopUser(folder, related string) error {
	var products []relatedProduct
	if related != "" {
		var err error
		if products, err = relatedProducts(related, msiProductInfo); err != nil {
			return err
		}
	}
	folders, notes, err := upgradeFolders(folder, products)
	if err != nil {
		return err
	}
	// A note that cannot be written does not skip the stop; it joins the result.
	var output error
	for _, note := range notes {
		output = errors.Join(output, printed("%s\n", note))
	}
	sid, err := currentUserSID()
	if err != nil {
		return errors.Join(fmt.Errorf("cannot identify the installing account: %w", err), output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), userStopBudget)
	defer cancel()
	stopped, err := stopUserPredecessors(ctx, folders, sid, systemUserStopOps())
	if err != nil {
		return errors.Join(err, output)
	}
	where := strings.Join(folders, "; ")
	if stopped == 0 {
		return errors.Join(output, printed("no jobd, jobdw or openabstractions process of this account is running under %s\n", where))
	}
	return errors.Join(output, printed("stopped %d predecessor process(es) of this account under %s; exit confirmed\n", stopped, where))
}

// withClosed adds a failure to close retained handles to err.
func withClosed(err, closeErr error) error {
	if closeErr == nil {
		return err
	}
	return errors.Join(err, fmt.Errorf("close retained process handles: %w", closeErr))
}

// userInstallFolder accepts the MSI-formatted install folder. The installer
// passes "[APPLICATIONFOLDER]." so the property's trailing backslash does not
// escape the closing quote; Clean removes the dot.
func userInstallFolder(folder string) (string, error) {
	if strings.ContainsRune(folder, '"') {
		return "", fmt.Errorf("install folder %q contains a quote; the command line was split incorrectly", folder)
	}
	if !filepath.IsAbs(folder) {
		return "", fmt.Errorf("install folder %q is not an absolute path", folder)
	}
	clean := filepath.Clean(folder)
	if filepath.Dir(clean) == clean {
		return "", fmt.Errorf("install folder %q is a volume root; refusing to match every process on it", folder)
	}
	return longPath(clean), nil
}

func stopUserPredecessors(ctx context.Context, folders []string, sid string, ops userStopOps) (count int, err error) {
	found, err := verifiedPredecessors(folders, sid, ops, nil)
	defer func() {
		var closeErr error
		for _, p := range found {
			closeErr = errors.Join(closeErr, p.process.close())
		}
		err = withClosed(err, closeErr)
	}()
	if err != nil {
		return 0, err
	}
	if ops.record != nil && len(found) > 0 {
		if err := ops.record(found); err != nil {
			return 0, fmt.Errorf("record predecessor processes before stopping them: %w", err)
		}
	}
	for _, p := range found {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("stopping predecessor processes: %w", err)
		}
		created, err := p.process.created()
		if err != nil {
			return 0, fmt.Errorf("re-query %s (pid %d): %w", p.image, p.pid, err)
		}
		if created != p.created {
			return 0, fmt.Errorf("predecessor %s (pid %d) changed identity before it was stopped; nothing further was terminated", p.image, p.pid)
		}
		if done, err := p.process.exited(); err != nil {
			return 0, fmt.Errorf("query %s (pid %d): %w", p.image, p.pid, err)
		} else if done {
			continue
		}
		if err := p.process.terminate(p.created); err != nil {
			if done, qerr := p.process.exited(); qerr == nil && done {
				continue
			}
			return 0, fmt.Errorf("terminate predecessor %s (pid %d): %w", p.image, p.pid, err)
		}
	}
	for {
		var running []string
		for _, p := range found {
			done, err := p.process.exited()
			if err != nil {
				return 0, fmt.Errorf("wait for %s (pid %d): %w", p.image, p.pid, err)
			}
			if !done {
				running = append(running, fmt.Sprintf("%s (pid %d)", p.image, p.pid))
			}
		}
		if len(running) == 0 {
			break
		}
		if err := waitRemoval(ctx); err != nil {
			return 0, fmt.Errorf("predecessor processes did not exit within %v: %s", userStopBudget, strings.Join(running, ", "))
		}
	}
	// A process started from the old folder during this stop is not covered by
	// the confirmed exits above; refuse instead of letting removal race it.
	stopped := map[uint32]int64{}
	for _, p := range found {
		stopped[p.pid] = p.created
	}
	late, err := verifiedPredecessors(folders, sid, ops, stopped)
	var closeErr error
	for _, p := range late {
		closeErr = errors.Join(closeErr, p.process.close())
	}
	if err != nil {
		return 0, withClosed(fmt.Errorf("cannot verify predecessor processes after shutdown: %w", err), closeErr)
	}
	if len(late) > 0 {
		return 0, withClosed(fmt.Errorf("predecessor process appeared during shutdown: %s (pid %d)", late[0].image, late[0].pid), closeErr)
	}
	return len(found), withClosed(nil, closeErr)
}

// verifiedPredecessors returns retained handles for every process of sid whose
// image is a jobd, jobdw or openabstractions executable inside folder. Processes
// in skip that match their recorded creation time, or that have exited, are
// omitted. The caller closes returned handles, including on error.
func verifiedPredecessors(folders []string, sid string, ops userStopOps, skip map[uint32]int64) ([]predecessor, error) {
	enumerated, entries, err := ops.snapshot()
	if err != nil {
		return nil, fmt.Errorf("cannot enumerate processes: %w", err)
	}
	var found []predecessor
	for _, entry := range entries {
		if !predecessorImages[strings.ToLower(entry.name)] {
			continue
		}
		process, err := ops.open(entry.pid)
		if errors.Is(err, errNotCandidate) {
			continue
		}
		if err != nil {
			return found, fmt.Errorf("open %s (pid %d): %w", entry.name, entry.pid, err)
		}
		match, image, created, err := matchPredecessor(process, folders, sid, enumerated)
		if err == nil && match && skip != nil {
			if recorded, ok := skip[entry.pid]; ok && recorded == created {
				match = false
			} else if done, qerr := process.exited(); qerr != nil {
				err = qerr
			} else if done {
				match = false
			}
		}
		if err != nil || !match {
			closeErr := process.close()
			if err != nil {
				return found, withClosed(fmt.Errorf("%s (pid %d): %w", entry.name, entry.pid, err), closeErr)
			}
			if closeErr != nil {
				return found, withClosed(nil, closeErr)
			}
			continue
		}
		found = append(found, predecessor{pid: entry.pid, image: image, created: created, process: process})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].pid < found[j].pid })
	return found, nil
}

// matchPredecessor reads identity only through the retained handle. A process
// created after the enumeration holds a reused PID and is refused outright.
func matchPredecessor(process heldProcess, folders []string, sid string, enumerated int64) (bool, string, int64, error) {
	image, err := process.image()
	if err != nil {
		if done, qerr := process.exited(); qerr == nil && done {
			return false, "", 0, nil
		}
		return false, "", 0, fmt.Errorf("image path: %w", err)
	}
	if !predecessorImages[strings.ToLower(filepath.Base(image))] || !insideAnyFolder(longPath(image), folders) {
		return false, image, 0, nil
	}
	owner, err := process.owner()
	if errors.Is(err, errNotCandidate) {
		return false, image, 0, nil
	}
	if err != nil {
		return false, image, 0, fmt.Errorf("owner: %w", err)
	}
	if !strings.EqualFold(owner, sid) {
		return false, image, 0, nil
	}
	created, err := process.created()
	if err != nil {
		return false, image, 0, fmt.Errorf("creation time: %w", err)
	}
	if created > enumerated {
		return false, image, 0, fmt.Errorf("process identity changed: %s was created after enumeration, so its PID was reused", image)
	}
	return true, image, created, nil
}

func insideAnyFolder(image string, folders []string) bool {
	for _, folder := range folders {
		if insideFolder(image, folder) {
			return true
		}
	}
	return false
}

func insideFolder(image, folder string) bool {
	image = filepath.Clean(image)
	prefix := strings.TrimRight(filepath.Clean(folder), `\/`) + `\`
	return len(image) > len(prefix) && strings.EqualFold(image[:len(prefix)], prefix)
}

func longPath(path string) string {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return path
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetLongPathName(p, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 || n > uint32(len(buf)) {
		return path
	}
	return windows.UTF16ToString(buf[:n])
}

func currentUserSID() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

func systemUserStopOps() userStopOps {
	env := systemExclusionEnv()
	return userStopOps{snapshot: processSnapshot, open: openHeldProcess,
		record: func(found []predecessor) error { return env.recordStopped(stoppedProcesses(found)) }}
}

func processSnapshot() (int64, []processEntry, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, nil, err
	}
	defer windows.CloseHandle(snap)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	var out []processEntry
	for err = windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
		out = append(out, processEntry{pid: entry.ProcessID, name: windows.UTF16ToString(entry.ExeFile[:])})
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return 0, nil, err
	}
	// Taken after the snapshot completes, so every listed process was created
	// no later than this instant.
	var now windows.Filetime
	windows.GetSystemTimePreciseAsFileTime(&now)
	return now.Nanoseconds(), out, nil
}

type systemHeldProcess struct {
	pid    uint32
	handle windows.Handle
}

func openHeldProcess(pid uint32) (heldProcess, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return nil, errNotCandidate
	}
	if err != nil {
		return nil, err
	}
	return systemHeldProcess{pid: pid, handle: h}, nil
}

func (p systemHeldProcess) close() error { return windows.CloseHandle(p.handle) }

func (p systemHeldProcess) exited() (bool, error) {
	return retainedServiceProcess{p.handle}.exited()
}

func (p systemHeldProcess) image() (string, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(p.handle, 0, &buf[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:size]), nil
}

func (p systemHeldProcess) owner() (string, error) {
	var token windows.Token
	err := windows.OpenProcessToken(p.handle, windows.TOKEN_QUERY, &token)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return "", errNotCandidate
	}
	if err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

func (p systemHeldProcess) created() (int64, error) {
	return handleCreated(p.handle)
}

func handleCreated(h windows.Handle) (int64, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0, err
	}
	return created.Nanoseconds(), nil
}

// terminate opens a second handle with terminate rights. The retained query
// handle keeps the PID from being reused, and the creation time check confirms
// the second handle names the same process object before it is used.
func (p systemHeldProcess) terminate(created int64) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, p.pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	actual, err := handleCreated(h)
	if err != nil {
		return err
	}
	if actual != created {
		return errors.New("process identity changed before termination")
	}
	return windows.TerminateProcess(h, 1)
}
