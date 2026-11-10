// Seeing past the launcher's wrappers.
//
// The guard reports a launcher's exit by watching the parent pid. That works
// when the launcher is the process that forked the watch — but `gh monitor`
// is normally started by the gh CLI, which forks the extension binary and
// *waits* for it. The watch's parent is then `gh`, and `gh` is what gets
// reparented when the session that started it exits: the watch's own parent
// never changes, so the guard never fires (issue #193). Two hundred leaked
// pollers over a weekend came from exactly that shape.
//
// So the baseline is not the parent but the first *non-wrapper* ancestor: the
// shell, editor, or agent that actually asked for the watch.
//
// When that ancestor has already exited, the OS has adopted the wrapper: the
// walk ends at init (pid 1, launchd on macOS). Init never exits, so a guard
// that waited for it to change would wait forever. Seven watches of that
// shape, up to four days old, were found polling on one machine. The walk
// therefore reports init as the launcher, and the guard reads that as the
// launcher being gone.

package orphan

// InitPID is the pid of init (launchd on macOS), the process the OS reparents
// an orphan to. A launcher resolved to it has already exited.
const InitPID = 1

// maxLauncherHops bounds the walk up the process tree. A wrapper chain longer
// than this is not a launcher worth following, and an unbounded walk on a
// corrupt or racing /proc can loop.
const maxLauncherHops = 4

// ParentFunc reports a process's parent pid and its command name. It returns
// ok=false when the process cannot be inspected.
type ParentFunc func(pid int) (ppid int, comm string, ok bool)

// ResolveLauncher walks up from pid past wrapper processes and returns the pid
// whose exit means the watch's launcher is gone.
//
// A wrapper is a process that exists only to run the watch — today, the gh
// CLI. Anything else is treated as the launcher, so a direct invocation from a
// shell resolves to the shell on the first hop, exactly as before. A chain
// that ends at init resolves to InitPID whether or not init itself can be
// inspected: the process that asked for the watch is already gone.
func ResolveLauncher(pid int, parentOf ParentFunc) int {
	if parentOf == nil {
		return 0
	}
	cur := pid
	for hop := 0; hop < maxLauncherHops; hop++ {
		ppid, _, ok := parentOf(cur)
		if !ok || ppid <= 0 {
			return 0
		}
		if ppid == InitPID {
			return InitPID
		}
		// The wrapper test asks about the parent's own command name: the
		// watch's own name says nothing about what started it.
		_, parentComm, pok := parentOf(ppid)
		if !pok || !isWrapper(parentComm) {
			return ppid
		}
		cur = ppid
	}
	return 0
}

// isWrapper reports whether comm names a process that forks the watch and
// waits for it, so its own exit is the launcher's exit.
func isWrapper(comm string) bool {
	switch comm {
	case "gh":
		return true
	}
	return false
}
