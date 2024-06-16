package sshd

import "log/slog"

var log = slog.Default()

func SetLogger(l *slog.Logger) {
	log = l
}
