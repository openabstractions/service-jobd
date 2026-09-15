package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// An upgrade exclusion keeps the installation being replaced from activating
// while a Windows Installer transaction owns it. The incoming package writes the
// record before it stops the predecessor, removes it when the transaction
// commits, and releases it during rollback just before restarting what the stop
// ended. Every activation path of the covered folders refuses while the record
// is live: `jobd start` (the Startup shortcut and StartUserRuntime), `jobd run`,
// the SCM-started `service run`, and `openabstractions start` through
// `service upgrade-check`.
//
// The record names its holder by the installer process's PID and creation time,
// so a transaction that ended without committing or rolling back stops excluding
// when that process is gone. The exclusion is not a lock: no OS lock outlives
// the short custom action processes that write it.
//
// A per-user upgrade run by a standard account executes begin-upgrade
// impersonated, and its parent is the SYSTEM-owned msiexec server, which that
// account cannot open. The helper then takes the parent's PID and image name
// from the process snapshot and records creation time 0, meaning "unreadable
// when written". Every reader treats such a holder the same way: a PID that is
// gone ends the exclusion, and a PID that is present, whether or not this
// reader can open it, is honoured only until exclusionUnverifiedLimit after the
// record began. PID reuse cannot be ruled out without a creation time, and the
// limit bounds what a reused PID can hold. The standard user's own activation
// paths get access denied on the holder, as the writer did; an elevated reader
// can open it but has nothing to compare, so both reach the same verdict. When
// the parent is readable, the record carries its creation time and full
// verification applies.

var errUpgradeInProgress = errors.New("an upgrade of this installation is in progress")

const (
	exclusionVersion  = 1
	exclusionMaxBytes = 64 << 10
	// A holder whose identity cannot be read (access denied) is honoured for
	// at most this long after the record was written.
	exclusionUnverifiedLimit = time.Hour
	// Administrators and SYSTEM control; Users read and traverse.
	machineExclusionSDDL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)"
)

type processIdentity struct {
	PID     uint32 `json:"pid"`
	Created int64  `json:"created"`
	Image   string `json:"image"`
}

type stoppedProcess struct {
	Image   string `json:"image"`
	PID     uint32 `json:"pid"`
	Created int64  `json:"created"`
}

type upgradeExclusion struct {
	Version   int              `json:"version"`
	Scope     string           `json:"scope"`
	Folders   []string         `json:"folders"`
	Installer processIdentity  `json:"installer"`
	Begun     time.Time        `json:"begun"`
	Stopped   []stoppedProcess `json:"stopped"`
	Services  []string         `json:"services"`
}

func decodeExclusion(data []byte) (*upgradeExclusion, error) {
	if len(data) > exclusionMaxBytes {
		return nil, errors.New("upgrade exclusion record is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record upgradeExclusion
	if err := decoder.Decode(&record); err != nil {
		return nil, fmt.Errorf("upgrade exclusion record: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("upgrade exclusion record has trailing data")
	}
	if err := record.validate(); err != nil {
		return nil, err
	}
	return &record, nil
}

func (r *upgradeExclusion) validate() error {
	if r.Version != exclusionVersion {
		return fmt.Errorf("upgrade exclusion record version %d is not %d", r.Version, exclusionVersion)
	}
	if r.Scope != "user" && r.Scope != "machine" {
		return fmt.Errorf("upgrade exclusion scope %q is not user or machine", r.Scope)
	}
	if len(r.Folders) == 0 {
		return errors.New("upgrade exclusion record names no folder")
	}
	for _, folder := range r.Folders {
		if !filepath.IsAbs(folder) || filepath.Clean(folder) != folder || filepath.Dir(folder) == folder {
			return fmt.Errorf("upgrade exclusion folder %q is not a clean absolute non-root path", folder)
		}
	}
	// Created 0 is a holder that was unreadable when the record was written.
	if r.Installer.PID == 0 || r.Installer.Created < 0 {
		return errors.New("upgrade exclusion record names no installer process")
	}
	if r.Begun.IsZero() {
		return errors.New("upgrade exclusion record has no start time")
	}
	for _, p := range r.Stopped {
		if !filepath.IsAbs(p.Image) || p.PID == 0 || p.Created <= 0 {
			return errors.New("upgrade exclusion record has an invalid stopped process")
		}
	}
	for _, name := range r.Services {
		if !strings.HasPrefix(name, serviceName+"_") || len(name) == len(serviceName)+1 {
			return fmt.Errorf("upgrade exclusion record names service %q, which is not a supervisor instance", name)
		}
	}
	return nil
}

func (r *upgradeExclusion) covers(image string) bool {
	return insideAnyFolder(longPath(image), r.Folders)
}

// exclusionEnv is the record's storage, trust and liveness. Tests substitute
// each part; systemExclusionEnv is the installed behaviour.
type exclusionEnv struct {
	path    func(scope string) (string, error)
	read    func(path string) ([]byte, error)
	write   func(path, scope string, data []byte) error
	remove  func(path string) error
	trusted func(path, scope string) error
	alive   func(holder processIdentity, begun time.Time) bool
	now     func() time.Time
}

func systemExclusionEnv() exclusionEnv {
	return exclusionEnv{
		path:    exclusionPath,
		read:    readExclusionFile,
		write:   writeExclusionFile,
		remove:  removeExclusionFile,
		trusted: trustedExclusionFile,
		alive:   func(holder processIdentity, begun time.Time) bool { return processAlive(holder, begun, time.Now()) },
		now:     time.Now,
	}
}

// load returns nil when no record exists. An error means a record exists and
// cannot be trusted or read.
func (e exclusionEnv) load(scope string) (*upgradeExclusion, string, error) {
	path, err := e.path(scope)
	if err != nil {
		return nil, "", err
	}
	data, err := e.read(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, path, nil
	}
	if err != nil {
		return nil, path, err
	}
	if err := e.trusted(path, scope); err != nil {
		return nil, path, err
	}
	record, err := decodeExclusion(data)
	if err != nil {
		return nil, path, err
	}
	if record.Scope != scope {
		return nil, path, fmt.Errorf("upgrade exclusion record %s is for scope %s", path, record.Scope)
	}
	return record, path, nil
}

func (e exclusionEnv) store(scope string, record *upgradeExclusion) error {
	if err := record.validate(); err != nil {
		return err
	}
	path, err := e.path(scope)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return e.write(path, scope, append(data, '\n'))
}

// active is the live record covering image, if any. Records that cannot be
// trusted or read are reported in notes and exclude nothing.
func (e exclusionEnv) active(image string) (*upgradeExclusion, []string) {
	var notes []string
	for _, scope := range []string{"user", "machine"} {
		record, path, err := e.load(scope)
		if err != nil {
			notes = append(notes, fmt.Sprintf("ignored upgrade exclusion %s: %v", path, err))
			continue
		}
		if record == nil || !record.covers(image) {
			continue
		}
		if !e.alive(record.Installer, record.Begun) {
			notes = append(notes, fmt.Sprintf("ignored upgrade exclusion %s: installer process %d is gone", path, record.Installer.PID))
			continue
		}
		return record, notes
	}
	return nil, notes
}

func (e exclusionEnv) begin(scope string, folders []string, holder processIdentity) error {
	existing, _, err := e.load(scope)
	if err == nil && existing != nil && existing.Installer != holder && e.alive(existing.Installer, existing.Begun) {
		return fmt.Errorf("installer process %d already holds the %s upgrade exclusion for %s",
			existing.Installer.PID, scope, strings.Join(existing.Folders, "; "))
	}
	return e.store(scope, &upgradeExclusion{
		Version: exclusionVersion, Scope: scope, Folders: folders, Installer: holder, Begun: e.now().UTC(),
		Stopped: []stoppedProcess{}, Services: []string{},
	})
}

// recordStopped adds processes to the user record before they are stopped. No
// record means the stop is not part of an installer transaction.
func (e exclusionEnv) recordStopped(processes []stoppedProcess) error {
	record, _, err := e.load("user")
	if err != nil || record == nil {
		return err
	}
	for _, p := range processes {
		known := false
		for _, have := range record.Stopped {
			if have.PID == p.PID && have.Created == p.Created {
				known = true
				break
			}
		}
		if !known {
			record.Stopped = append(record.Stopped, p)
		}
	}
	return e.store("user", record)
}

// recordService adds a supervisor instance to the machine record before its
// stop control is sent.
func (e exclusionEnv) recordService(name string) error {
	record, _, err := e.load("machine")
	if err != nil || record == nil {
		return err
	}
	for _, have := range record.Services {
		if strings.EqualFold(have, name) {
			return nil
		}
	}
	record.Services = append(record.Services, name)
	return e.store("machine", record)
}

// release removes the record and returns what it held. Rollback releases before
// restarting, so the restarted processes are not refused by their own record.
func (e exclusionEnv) release(scope string) (*upgradeExclusion, []string) {
	var notes []string
	record, path, err := e.load(scope)
	if err != nil {
		notes = append(notes, fmt.Sprintf("upgrade exclusion %s was unreadable, so nothing it recorded is restarted: %v", path, err))
	}
	if path != "" {
		if err := e.remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			notes = append(notes, fmt.Sprintf("could not remove upgrade exclusion %s: %v", path, err))
		}
	}
	if record == nil && err == nil {
		notes = append(notes, fmt.Sprintf("no %s upgrade exclusion was recorded, so nothing is restarted", scope))
	}
	return record, notes
}

// recordedIn reports whether the record stopped any process whose image lies in
// folder.
func (r *upgradeExclusion) recordedIn(folder string) bool {
	if r == nil {
		return false
	}
	for _, p := range r.Stopped {
		if insideFolder(longPath(p.Image), folder) {
			return true
		}
	}
	return false
}

// restartRecordedServices starts exactly the supervisor instances the machine
// stop recorded. An instance that no longer exists is named; an instance that
// is already running counts as restored.
func restartRecordedServices(record *upgradeExclusion, start func(string) error) (int, []string, error) {
	if record == nil {
		return 0, nil, nil
	}
	var notes []string
	var failures []error
	started := 0
	for _, name := range record.Services {
		err := start(name)
		switch {
		case err == nil, errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING):
			started++
		case errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST):
			notes = append(notes, fmt.Sprintf("supervisor instance %s no longer exists; it was not restarted", name))
		default:
			failures = append(failures, fmt.Errorf("start supervisor instance %s: %w", name, err))
		}
	}
	return started, notes, errors.Join(failures...)
}

func refuseDuringUpgrade() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot check the upgrade exclusion without this program's path: %w", err)
	}
	return systemExclusionEnv().refusal(self)
}

func (e exclusionEnv) refusal(image string) error {
	record, _ := e.active(image)
	if record == nil {
		return nil
	}
	return fmt.Errorf("%w: %s is being replaced by installer process %d since %s; activation resumes when that installation finishes",
		errUpgradeInProgress, strings.Join(record.Folders, "; "), record.Installer.PID, record.Begun.Format(time.RFC3339))
}

// Commands the installer runs.

// printed writes command output and returns the write error.
func printed(format string, args ...any) error {
	_, err := fmt.Printf(format, args...)
	return err
}

func serviceBeginUpgrade(scope, folder, related string) error {
	info := msiProductInfo
	if scope == "machine" {
		info = msiMachineProductInfo
	}
	products, err := relatedProducts(related, info)
	if err != nil {
		return err
	}
	folders, notes, err := upgradeFolders(folder, products)
	if err != nil {
		return err
	}
	for _, note := range notes {
		if err := printed("%s\n", note); err != nil {
			return err
		}
	}
	holder, err := installerIdentity()
	if err != nil {
		return fmt.Errorf("cannot identify the installer that holds the upgrade exclusion: %w", err)
	}
	if err := systemExclusionEnv().begin(scope, folders, holder); err != nil {
		return err
	}
	return printed("%s upgrade exclusion held by %s (pid %d) for %s\n", scope, holder.Image, holder.PID, strings.Join(folders, "; "))
}

func serviceEndUpgrade(scope string) error {
	path, err := exclusionPath(scope)
	if err != nil {
		return err
	}
	if err := removeExclusionFile(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return printed("%s upgrade exclusion released\n", scope)
}

func serviceUpgradeCheck() error {
	if err := refuseDuringUpgrade(); err != nil {
		return err
	}
	return printed("no upgrade of this installation is in progress\n")
}

// serviceStartMachine restarts the recorded services even when a note cannot be
// written; a failed write is joined into the result instead.
func serviceStartMachine() error {
	record, notes := systemExclusionEnv().release("machine")
	var output error
	for _, note := range notes {
		output = errors.Join(output, printed("%s\n", note))
	}
	started, more, err := restartRecordedServices(record, startSupervisorInstance)
	for _, note := range more {
		output = errors.Join(output, printed("%s\n", note))
	}
	output = errors.Join(output, printed("restarted %d recorded supervisor instance(s)\n", started))
	return errors.Join(err, output)
}

func startSupervisorInstance(name string) error {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(m)
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	h, err := windows.OpenService(m, p, windows.SERVICE_START)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(h)
	return windows.StartService(h, 0, nil)
}

// Storage, trust and liveness on Windows.

func exclusionPath(scope string) (string, error) {
	switch scope {
	case "user":
		base, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
		if err != nil {
			return "", err
		}
		return filepath.Join(base, "openabstractions", "upgrade-v1", "exclusion.json"), nil
	case "machine":
		base, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
		if err != nil {
			return "", err
		}
		return filepath.Join(base, "abstraction", "upgrade-v1", "exclusion.json"), nil
	}
	return "", fmt.Errorf("upgrade exclusion scope %q is not user or machine", scope)
}

func readExclusionFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("upgrade exclusion %s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, exclusionMaxBytes+1))
}

func removeExclusionFile(path string) error {
	return os.Remove(path)
}

func writeExclusionFile(path, scope string, data []byte) error {
	dir := filepath.Dir(path)
	if scope == "machine" {
		if err := secureMachineDirectories(dir); err != nil {
			return err
		}
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "exclusion-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		var from, to *uint16
		if from, err = windows.UTF16PtrFromString(name); err == nil {
			if to, err = windows.UTF16PtrFromString(path); err == nil {
				err = windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
			}
		}
	}
	if err != nil {
		if removeErr := os.Remove(name); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove temporary exclusion %s: %w", name, removeErr))
		}
	}
	return err
}

// secureMachineDirectories creates %ProgramData%\abstraction and its upgrade
// directory with an administrator-controlled ACL, and refuses either one when
// something other than SYSTEM or Administrators owns it.
func secureMachineDirectories(dir string) error {
	sd, err := windows.SecurityDescriptorFromString(machineExclusionSDDL)
	if err != nil {
		return err
	}
	attributes := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	for _, d := range []string{filepath.Dir(dir), dir} {
		p, err := windows.UTF16PtrFromString(d)
		if err != nil {
			return err
		}
		if err := windows.CreateDirectory(p, attributes); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return err
		}
		if err := requireAdministrativeOwner(d); err != nil {
			return err
		}
	}
	return nil
}

func pathOwner(path string) (*windows.SID, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	owner, _, err := sd.Owner()
	return owner, err
}

func administrativeSID(sid *windows.SID) bool {
	return sid != nil && (sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid))
}

func requireAdministrativeOwner(path string) error {
	owner, err := pathOwner(path)
	if err != nil {
		return err
	}
	if !administrativeSID(owner) {
		return fmt.Errorf("%s is owned by %s, not by SYSTEM or Administrators", path, owner)
	}
	return nil
}

// trustedExclusionFile accepts a machine record only from SYSTEM or
// Administrators, and a user record from those or the current user. A record a
// standard user wrote cannot block another account's activation.
func trustedExclusionFile(path, scope string) error {
	owner, err := pathOwner(path)
	if err != nil {
		return err
	}
	if administrativeSID(owner) {
		return nil
	}
	if scope == "user" {
		sid, err := currentUserSID()
		if err == nil && strings.EqualFold(owner.String(), sid) {
			return nil
		}
	}
	return fmt.Errorf("owned by %s, which cannot hold the %s upgrade exclusion", owner, scope)
}

// processAlive reports whether holder still names the same running process. A
// holder recorded with creation time 0 is never compared: it is honoured while
// its PID exists and the record is younger than exclusionUnverifiedLimit.
func processAlive(holder processIdentity, begun, now time.Time) bool {
	withinLimit := now.Sub(begun) < exclusionUnverifiedLimit
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, holder.PID)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return false
	}
	if err != nil {
		return withinLimit
	}
	defer windows.CloseHandle(h)
	if holder.Created == 0 {
		state, err := windows.WaitForSingleObject(h, 0)
		return withinLimit && err == nil && state == uint32(windows.WAIT_TIMEOUT)
	}
	created, err := handleCreated(h)
	if err != nil {
		return withinLimit
	}
	if created != holder.Created {
		return false
	}
	state, err := windows.WaitForSingleObject(h, 0)
	return err == nil && state == uint32(windows.WAIT_TIMEOUT)
}

// identityOps are the process queries installerIdentity makes. Tests substitute
// them; systemIdentityOps is the installed behaviour.
type identityOps struct {
	self func() uint32
	// parent returns the parent PID and image name from the process snapshot.
	parent func(pid uint32) (uint32, string, error)
	// created is this process's creation time.
	created func() (int64, error)
	// inspect opens pid and returns its creation time and full image path. An
	// OpenProcess failure is returned as *openProcessError.
	inspect func(pid uint32) (int64, string, error)
}

type openProcessError struct {
	pid uint32
	err error
}

func (e *openProcessError) Error() string {
	return fmt.Sprintf("open parent process %d: %v", e.pid, e.err)
}
func (e *openProcessError) Unwrap() error { return e.err }

func systemIdentityOps() identityOps {
	return identityOps{
		self:    func() uint32 { return uint32(os.Getpid()) },
		parent:  parentProcess,
		created: func() (int64, error) { return handleCreated(windows.CurrentProcess()) },
		inspect: inspectProcess,
	}
}

func installerIdentity() (processIdentity, error) {
	return installerIdentityWith(systemIdentityOps())
}

// installerIdentityWith is this custom action's parent: the Windows Installer
// process running the transaction script. A parent created after this process
// means its PID was reused, and is refused. A parent this account may not open
// (the SYSTEM msiexec server above an impersonated action) is named by its
// snapshot PID and image with creation time 0, and the reuse check is skipped
// only then; the file comment gives how readers treat that record.
func installerIdentityWith(ops identityOps) (processIdentity, error) {
	parent, name, err := ops.parent(ops.self())
	if err != nil {
		return processIdentity{}, err
	}
	own, err := ops.created()
	if err != nil {
		return processIdentity{}, err
	}
	created, image, err := ops.inspect(parent)
	var denied *openProcessError
	if errors.As(err, &denied) && errors.Is(denied.err, windows.ERROR_ACCESS_DENIED) {
		return processIdentity{PID: parent, Created: 0, Image: name}, nil
	}
	if err != nil {
		return processIdentity{}, err
	}
	if created <= 0 {
		return processIdentity{}, fmt.Errorf("parent process %d reported no creation time", parent)
	}
	if created > own {
		return processIdentity{}, fmt.Errorf("parent process %d was created after this process; its PID was reused", parent)
	}
	return processIdentity{PID: parent, Created: created, Image: image}, nil
}

func inspectProcess(pid uint32) (int64, string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, "", &openProcessError{pid: pid, err: err}
	}
	defer windows.CloseHandle(h)
	created, err := handleCreated(h)
	if err != nil {
		return 0, "", err
	}
	image, err := (systemHeldProcess{pid: pid, handle: h}).image()
	if err != nil {
		return 0, "", err
	}
	return created, image, nil
}

func parentProcess(pid uint32) (uint32, string, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, "", err
	}
	defer windows.CloseHandle(snap)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
		if entry.ProcessID == pid {
			if entry.ParentProcessID == 0 {
				return 0, "", fmt.Errorf("process %d has no parent", pid)
			}
			parent := entry.ParentProcessID
			// The parent's own entry carries its image name.
			for err = windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
				if entry.ProcessID == parent {
					return parent, windows.UTF16ToString(entry.ExeFile[:]), nil
				}
			}
			return 0, "", fmt.Errorf("parent process %d of %d is not in the process snapshot", parent, pid)
		}
	}
	return 0, "", fmt.Errorf("process %d is not in the process snapshot", pid)
}

func stoppedProcesses(found []predecessor) []stoppedProcess {
	out := make([]stoppedProcess, 0, len(found))
	for _, p := range found {
		out = append(out, stoppedProcess{Image: p.image, PID: p.pid, Created: p.created})
	}
	return out
}
