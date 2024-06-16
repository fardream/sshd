package sshd

import "golang.org/x/sys/unix"

func setWindowSize(fd int, row, col uint16) error {
	if row == 0 || col == 0 {
		return nil
	}

	return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{
		Row: row,
		Col: col,
	})
}
