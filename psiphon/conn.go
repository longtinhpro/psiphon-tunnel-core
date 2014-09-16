/*
 * Copyright (c) 2014, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package psiphon

import (
	"errors"
	"net"
	"os"
	"syscall"
	"time"
)

// Conn is a customized network connection that:
// - can be interrupted while connecting;
// - turns on TCP keep alive;
// - implements idle read/write timeouts;
// - supports sending a signal to a channel when it disconnects;
// - can be bound to a specific system device (for Android VpnService
//   routing compatibility, for example).
type Conn struct {
	net.Conn
	socketFd            int
	needCloseSocketFd   bool
	isDisconnected      bool
	disconnectionSignal chan bool
	readTimeout         time.Duration
	writeTimeout        time.Duration
}

// NewConn creates a new, configured Conn. Unlike standard Dial
// functions, this does not return a connected net.Conn. Call the Connect function
// to complete the connection establishment. To implement device binding and
// interruptible connecting, the lower-level syscall APIs are used.
func NewConn(readTimeout, writeTimeout time.Duration, deviceName string) (*Conn, error) {
	socketFd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	err = syscall.SetsockoptInt(socketFd, syscall.IPPROTO_TCP, syscall.TCP_KEEPALIVE, TCP_KEEP_ALIVE_PERIOD_SECONDS)
	if err != nil {
		syscall.Close(socketFd)
		return nil, err
	}
	if deviceName != "" {
		// TODO: requires root, which we won't have on Android in VpnService mode
		//       an alternative may be to use http://golang.org/pkg/syscall/#UnixRights to
		//       send the fd to the main Android process which receives the fd with
		//       http://developer.android.com/reference/android/net/LocalSocket.html#getAncillaryFileDescriptors%28%29
		//       and then calls
		//       http://developer.android.com/reference/android/net/VpnService.html#protect%28int%29.
		//       See, for example:
		//       https://code.google.com/p/ics-openvpn/source/browse/main/src/main/java/de/blinkt/openvpn/core/OpenVpnManagementThread.java#164
		const SO_BINDTODEVICE = 0x19 // only defined for Linux
		err = syscall.SetsockoptString(socketFd, syscall.SOL_SOCKET, SO_BINDTODEVICE, deviceName)
		return nil, err
	}
	return &Conn{
		socketFd:          socketFd,
		needCloseSocketFd: true,
		readTimeout:       readTimeout,
		writeTimeout:      writeTimeout}, nil
}

// Connect establishes a connection to the specified host. The sequence of
// syscalls in this implementation are taken from: https://code.google.com/p/go/issues/detail?id=6966
func (conn *Conn) Connect(ipAddress string, port int) (err error) {
	// TODO: domain name resolution (for meek)
	var addr [4]byte
	copy(addr[:], net.ParseIP(ipAddress).To4())
	sockAddr := syscall.SockaddrInet4{Addr: addr, Port: port}
	err = syscall.Connect(conn.socketFd, &sockAddr)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(conn.socketFd), "")
	defer file.Close()
	fileConn, err := net.FileConn(file)
	if err != nil {
		return err
	}
	conn.Conn = fileConn
	conn.needCloseSocketFd = false
	return nil
}

// SetDisconnectionSignal sets the channel which will be signaled
// when the connection terminates. This function returns an error
// if the connection is already disconnected (and would never send
// the signal).
func (conn *Conn) SetDisconnectionSignal(disconnectionSignal chan bool) (err error) {
	if conn.isDisconnected {
		return errors.New("connection is already disconnected")
	}
	conn.disconnectionSignal = disconnectionSignal
	return nil
}

// Close terminates down an established (net.Conn) or establishing (socketFd) connection.
func (conn *Conn) Close() (err error) {
	if conn.needCloseSocketFd {
		err = syscall.Close(conn.socketFd)
		conn.needCloseSocketFd = false
	}
	if conn.Conn != nil {
		err = conn.Conn.Close()
	}
	if conn.disconnectionSignal != nil {
		select {
		case conn.disconnectionSignal <- true:
		default:
		}
	}
	conn.isDisconnected = true
	return err
}

// Read wraps standard Read to add an idle timeout. The connection
// is explicitly terminated on timeout.
func (conn *Conn) Read(buffer []byte) (n int, err error) {
	if conn.Conn == nil {
		return 0, errors.New("not connected")
	}
	if conn.readTimeout != 0 {
		err = conn.Conn.SetReadDeadline(time.Now().Add(conn.readTimeout))
		if err != nil {
			return 0, err
		}
	}
	n, err = conn.Conn.Read(buffer)
	if err != nil {
		conn.Close()
	}
	return
}

// Write wraps standard Write to add an idle timeout The connection
// is explicitly terminated on timeout.
func (conn *Conn) Write(buffer []byte) (n int, err error) {
	if conn.Conn == nil {
		return 0, errors.New("not connected")
	}
	if conn.writeTimeout != 0 {
		err = conn.Conn.SetWriteDeadline(time.Now().Add(conn.writeTimeout))
		if err != nil {
			return 0, err
		}
	}
	n, err = conn.Conn.Write(buffer)
	if err != nil {
		conn.Close()
	}
	return
}