package redirfd

import (
	"fmt"
	"os"
	"syscall"
)

type RedirectMode int

const (
	Read           RedirectMode = iota // open file for reading
	Write                              // open file for writing, truncating if it exists
	Update                             // open file for read & write
	Append                             // open file for append, create if it does not exist
	AppendExisting                     // open file for append, do not create if it does not already exist
	WriteNew                           // open file for writing, creating it, failing if it already exists
)

// see https://github.com/skarnet/execline/blob/master/src/execline/redirfd.c
func (mode RedirectMode) Redirect(nonblock, changemode bool, fd int, name string) (*os.File, error) {
	flags := 0
	what := -1

	switch mode {
	case Read:
		what = syscall.O_RDONLY
		flags &= ^(syscall.O_APPEND | syscall.O_CREAT | syscall.O_TRUNC | syscall.O_EXCL)
	case Write:
		what = syscall.O_WRONLY
		flags |= syscall.O_CREAT | syscall.O_TRUNC
		flags &= ^(syscall.O_APPEND | syscall.O_EXCL)
	case Update:
		what = syscall.O_RDWR
		flags &= ^(syscall.O_APPEND | syscall.O_CREAT | syscall.O_TRUNC | syscall.O_EXCL)
	case Append:
		what = syscall.O_WRONLY
		flags |= syscall.O_CREAT | syscall.O_APPEND
		flags &= ^(syscall.O_TRUNC | syscall.O_EXCL)
	case AppendExisting:
		what = syscall.O_WRONLY
		flags |= syscall.O_APPEND
		flags &= ^(syscall.O_CREAT | syscall.O_TRUNC | syscall.O_EXCL)
	case WriteNew:
		what = syscall.O_WRONLY
		flags |= syscall.O_CREAT | syscall.O_EXCL
		flags &= ^(syscall.O_APPEND | syscall.O_TRUNC)
	default:
		return nil, fmt.Errorf("unexpected mode %d", mode)
	}
	if nonblock {
		flags |= syscall.O_NONBLOCK
	}
	flags |= what

	fd2, e := syscall.Open(name, flags, 0666)
	if (what == syscall.O_WRONLY) && (e == syscall.ENXIO) {
		// Opens file in read-only, non-blocking mode. Returns a valid fd number if it succeeds, or -1 (and sets errno) if it fails.
		fdr, e2 := syscall.Open(name, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
		if e2 != nil {
			return nil, &os.PathError{"open_read", name, e2}
		}
		fd2, e = syscall.Open(name, flags, 0666)
		fd_close(fdr)
	}
	if e != nil {
		return nil, &os.PathError{"open", name, e}
	}
	if e = fd_move(fd, fd2); e != nil {
		return nil, &os.PathError{"fd_move", name, e}
	}
	if changemode {
		if nonblock {
			e = ndelay_off(fd)
		} else {
			e = ndelay_on(fd)
		}
		if e != nil {
			return nil, &os.PathError{"ndelay", name, e}
		}
	}
	return os.NewFile(uintptr(fd2), name), nil
}

// see https://github.com/skarnet/skalibs/blob/master/src/libstddjb/fd_move.c
func fd_move(to, from int) (err error) {
	if to == from {
		return
	}
	for {
		_, _, e1 := syscall.RawSyscall(syscall.SYS_DUP2, uintptr(from), uintptr(to), 0)
		if e1 != syscall.EINTR {
			if e1 != 0 {
				err = e1
			}
			break
		}
	}
	if err != nil {
		err = fd_close(from)
	}
	return
	/*
	   do
	     r = dup2(from, to) ;
	   while ((r == -1) && (errno == EINTR)) ;
	   return (r == -1) ? -1 : fd_close(from) ;
	*/
}

// see https://github.com/skarnet/skalibs/blob/master/src/libstddjb/fd_close.c
func fd_close(fd int) (err error) {
	i := 0
	var e error
	for {
		if e = syscall.Close(fd); e != nil {
			return nil
		}
		i++
		if e != syscall.EINTR {
			break
		}
	}
	if e == syscall.EBADF && i > 1 {
		return nil
	}
	return e
}

/*
int fd_close (int fd)
{
  register unsigned int i = 0 ;
doit:
  if (!close(fd)) return 0 ;
  i++ ;
  if (errno == EINTR) goto doit ;
  return ((errno == EBADF) && (i > 1)) ? 0 : -1 ;
}
*/

// see https://github.com/skarnet/skalibs/blob/master/src/libstddjb/ndelay_on.c
func ndelay_on(fd int) error {
	// 32-bit will likely break because it needs SYS_FCNTL64
	got, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFL), 0)
	if e != 0 {
		return e
	}
	_, _, e = syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_SETFL), uintptr(got|syscall.O_NONBLOCK))
	if e != 0 {
		return e
	}
	return nil
}

/*
int ndelay_on (int fd)
{
  register int got = fcntl(fd, F_GETFL) ;
  return (got == -1) ? -1 : fcntl(fd, F_SETFL, got | O_NONBLOCK) ;
}
*/

// see https://github.com/skarnet/skalibs/blob/master/src/libstddjb/ndelay_off.c
func ndelay_off(fd int) error {
	// 32-bit will likely break because it needs SYS_FCNTL64
	got, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFL), 0)
	if e != 0 {
		return e
	}
	_, _, e = syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_SETFL), uintptr(int(got) & ^syscall.O_NONBLOCK))
	if e != 0 {
		return e
	}
	return nil
}

/*
int ndelay_off (int fd)
{
  register int got = fcntl(fd, F_GETFL) ;
  return (got == -1) ? -1 : fcntl(fd, F_SETFL, got & ^O_NONBLOCK) ;
}
*/
