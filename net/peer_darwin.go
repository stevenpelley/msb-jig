package netstack

import (
	"net"

	"golang.org/x/sys/unix"
)

func peerUID(c *net.UnixConn) (int, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return -1, err
	}
	uid := -1
	var serr error
	err = rc.Control(func(fd uintptr) {
		var cred *unix.Xucred
		cred, serr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if serr == nil {
			uid = int(cred.Uid)
		}
	})
	if err == nil {
		err = serr
	}
	return uid, err
}
