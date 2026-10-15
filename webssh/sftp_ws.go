//go:build webssh

package webssh

// handleFileMsg dispatches file panel messages (0x04-0x0B) over the
// session's SFTP client. Skeleton only; filled in by the SFTP task.
func (s *wsSession) handleFileMsg(typ byte, payload []byte) {}
