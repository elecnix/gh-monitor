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
// walk ends at init (pid 1: systemd, launchd, or a container init such as
// tini). Init never exits, so a guard that waited for it to change would wait
// forever. Seven watches of that shape, up to four days old, were found
// polling on one machine. The walk therefore reports LauncherGone when the
// chain ends at an init, and the guard fires on it. A pid 1 that is not an
// init is a container entrypoint that started the watch itself, such as a
// shell or an agent: it is a live launcher, and its exit ends the whole pid
// namespace, so nothing is leaked by waiting on it.

package orphan

// LauncherGone is what ResolveLauncher reports when the chain ends at init:
// the process that asked for the watch has already exited.
const LauncherGone = -1

// initPID is the pid the OS reparents an orphan to.
const initPID = 1

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
// that ends at pid 1 resolves to LauncherGone when pid 1 is an init, or
// cannot be inspected at all: the process that asked for the watch is
// already gone. A pid 1 with any other name is a container entrypoint and
// resolves as a live launcher.
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
		// The wrapper test asks about the parent's own command name: the
		// watch's own name says nothing about what started it.
		_, parentComm, pok := parentOf(ppid)
		if ppid == initPID && (!pok || isInit(parentComm)) {
			return LauncherGone
		}
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

// isInit reports whether comm names a process that adopts orphans and never
// exits on its own: the system init, or a container init that only
// supervises its children.
func isInit(comm string) bool {
	switch comm {
	case "init", "systemd", "launchd", "tini", "docker-init", "dumb-init", "s6-svscan":
		return true
	}
	return false
}
