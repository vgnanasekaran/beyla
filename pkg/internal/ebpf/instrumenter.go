//go:build linux

package ebpf

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os"
	"syscall"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/prometheus/procfs"
	"golang.org/x/sys/unix"

	ebpfcommon "github.com/grafana/beyla/pkg/internal/ebpf/common"
	"github.com/grafana/beyla/pkg/internal/exec"
	"github.com/grafana/beyla/pkg/internal/goexec"
)

type instrumenter struct {
	offsets   *goexec.Offsets
	exe       *link.Executable
	closables []io.Closer
}

func ilog() *slog.Logger {
	return slog.With("component", "ebpf.Instrumenter")
}

func (i *instrumenter) goprobes(p Tracer) error {
	log := ilog().With("probes", "goprobes")
	// TODO: not running program if it does not find the required probes
	for funcName, funcPrograms := range p.GoProbes() {
		offs, ok := i.offsets.Funcs[funcName]
		if !ok {
			// the program function is not in the detected offsets. Ignoring
			log.Debug("ignoring function", "function", funcName)
			continue
		}
		log.Debug("going to instrument function", "function", funcName, "offsets", offs, "programs", funcPrograms)
		if err := i.goprobe(ebpfcommon.Probe{
			Offsets:  offs,
			Programs: funcPrograms,
		}); err != nil {
			return fmt.Errorf("instrumenting function %q: %w", funcName, err)
		}
		p.AddCloser(i.closables...)
	}

	return nil
}

func (i *instrumenter) goprobe(probe ebpfcommon.Probe) error {
	// Attach BPF programs as start and return probes
	if probe.Programs.Start != nil {
		up, err := i.exe.Uprobe("", probe.Programs.Start, &link.UprobeOptions{
			Address: probe.Offsets.Start,
		})
		if err != nil {
			return fmt.Errorf("setting uprobe: %w", err)
		}
		i.closables = append(i.closables, up)
	}

	if probe.Programs.End != nil {
		// Go won't work with Uretprobes because of the way Go manages the stack. We need to set uprobes just before the return
		// values: https://github.com/iovisor/bcc/issues/1320
		for _, ret := range probe.Offsets.Returns {
			urp, err := i.exe.Uprobe("", probe.Programs.End, &link.UprobeOptions{
				Address: ret,
			})
			if err != nil {
				return fmt.Errorf("setting uretprobe: %w", err)
			}
			i.closables = append(i.closables, urp)
		}
	}

	return nil
}

func (i *instrumenter) kprobes(p KprobesTracer) error {
	log := ilog().With("probes", "kprobes")
	for kfunc, kprobes := range p.KProbes() {
		log.Debug("going to add kprobe to function", "function", kfunc, "probes", kprobes)

		if err := i.kprobe(kfunc, kprobes); err != nil {
			return fmt.Errorf("instrumenting function %q: %w", kfunc, err)
		}
		p.AddCloser(i.closables...)
	}

	return nil
}

func (i *instrumenter) kprobe(funcName string, programs ebpfcommon.FunctionPrograms) error {
	if programs.Start != nil {
		kp, err := link.Kprobe(funcName, programs.Start, nil)
		if err != nil {
			return fmt.Errorf("setting kprobe: %w", err)
		}
		i.closables = append(i.closables, kp)
	}

	if programs.End != nil {
		// The commented code doesn't work on certain kernels. We need to invesigate more to see if it's possible
		// to productize it. Failure says: "neither debugfs nor tracefs are mounted".
		kp, err := link.Kretprobe(funcName, programs.End, nil /*&link.KprobeOptions{RetprobeMaxActive: 1024}*/)
		if err != nil {
			return fmt.Errorf("setting kretprobe: %w", err)
		}
		i.closables = append(i.closables, kp)
	}

	return nil
}

//nolint:cyclop
func (i *instrumenter) uprobes(pid int32, p Tracer) error {
	maps, err := processMaps(pid)
	if err != nil {
		return err
	}
	log := ilog().With("probes", "uprobes")
	if len(maps) == 0 {
		log.Info("didn't find any process maps, not instrumenting shared libraries", "pid", pid)
		return nil
	}

	for lib, pMap := range p.UProbes() {
		log.Debug("finding library", "lib", lib)
		libMap := exec.LibPath(lib, maps)
		instrPath := fmt.Sprintf("/proc/%d/exe", pid)

		ino := uint64(0)

		if libMap != nil {
			log.Debug("instrumenting library", "lib", lib, "path", libMap.Pathname)
			// we do this to make sure instrumenting something like libssl.so works with Docker
			instrPath = fmt.Sprintf("/proc/%d/map_files/%x-%x", pid, libMap.StartAddr, libMap.EndAddr)

			info, err := os.Stat(instrPath)
			if err == nil {
				stat, ok := info.Sys().(*syscall.Stat_t)
				if ok {
					if p.AlreadyInstrumentedLib(stat.Ino) {
						log.Debug("library already instrumented", "lib", lib, "path", libMap.Pathname, "ino", stat.Ino)
						continue
					}
					ino = stat.Ino
					log.Debug("found inode number, recording this instrumentation if successful", "lib", lib, "path", libMap.Pathname, "ino", ino)
				}
			}
		} else {
			// E.g. NodeJS uses OpenSSL but they ship it as statically linked in the node binary
			log.Debug(fmt.Sprintf("%s not linked, attempting to instrument executable", lib), "path", instrPath)
		}

		libExe, err := link.OpenExecutable(instrPath)

		if err != nil {
			return err
		}

		for funcName, funcPrograms := range pMap {
			log.Debug("going to instrument function", "function", funcName, "programs", funcPrograms)
			if err := i.uprobe(funcName, libExe, funcPrograms); err != nil {
				if funcPrograms.Required {
					return fmt.Errorf("instrumenting function %q: %w", funcName, err)
				}

				// error will be common here since this could be no openssl loaded
				log.Debug("error instrumenting uprobe", "function", funcName, "error", err)
			}
			p.AddCloser(i.closables...)
		}

		if ino != 0 {
			p.RecordInstrumentedLib(ino)
		}
	}

	return nil
}

func (i *instrumenter) uprobe(funcName string, exe *link.Executable, probe ebpfcommon.FunctionPrograms) error {
	if probe.Start != nil {
		up, err := exe.Uprobe(funcName, probe.Start, nil)
		if err != nil {
			return fmt.Errorf("setting uprobe: %w", err)
		}
		i.closables = append(i.closables, up)
	}

	if probe.End != nil {
		up, err := exe.Uretprobe(funcName, probe.End, nil)
		if err != nil {
			return fmt.Errorf("setting uretprobe: %w", err)
		}
		i.closables = append(i.closables, up)
	}

	return nil
}

func (i *instrumenter) sockfilters(p Tracer) error {
	for _, filter := range p.SocketFilters() {
		fd, err := attachSocketFilter(filter)
		if err != nil {
			return fmt.Errorf("attaching socket filter: %w", err)
		}

		p.AddCloser(&ebpfcommon.Filter{Fd: fd})
	}

	return nil
}

func attachSocketFilter(filter *ebpf.Program) (int, error) {
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ALL)))
	if err == nil {
		ssoErr := syscall.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ATTACH_BPF, filter.FD())
		if ssoErr != nil {
			return -1, ssoErr
		}
		return fd, nil
	}

	return -1, err
}

func isLittleEndian() bool {
	var a uint16 = 1

	return *(*byte)(unsafe.Pointer(&a)) == 1
}

func htons(a uint16) uint16 {
	if isLittleEndian() {
		var arr [2]byte
		binary.LittleEndian.PutUint16(arr[:], a)
		return binary.BigEndian.Uint16(arr[:])
	}
	return a
}

func processMaps(pid int32) ([]*procfs.ProcMap, error) {
	return exec.FindLibMaps(pid)
}
