package libcore

import (
	"context"
	"errors"
	"fmt"
	"github.com/v2fly/v2ray-core/v5/common/buf"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/sirupsen/logrus"
	v2rayNet "github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/features/dns"
	"github.com/v2fly/v2ray-core/v5/transport/internet"
	"golang.org/x/sys/unix"
	"libcore/comm"
)

type Protector interface {
	Protect(fd int32) bool
}

var noopProtectorInstance = &noopProtector{}

type noopProtector struct{}

func (n *noopProtector) Protect(int32) bool {
	return true
}

type protectedDialer struct {
	protector Protector
	resolver  func(domain string) ([]net.IP, error)
}

func (dialer protectedDialer) Dial(ctx context.Context, source v2rayNet.Address, destination v2rayNet.Destination, sockopt *internet.SocketConfig) (conn net.Conn, err error) {
	if destination.Network == v2rayNet.Network_Unknown || destination.Address == nil {
		buffer := buf.StackNew()
		buffer.Resize(0, int32(runtime.Stack(buffer.Extend(buf.Size), false)))
		logrus.Warn("connect to invalid destination:\n", buffer.String())
		buffer.Release()

		return nil, newError("invalid destination")
	}

	var ips []net.IP
	if destination.Address.Family().IsDomain() {
		ips, err = dialer.resolver(destination.Address.Domain())
		if err == nil && len(ips) == 0 {
			err = dns.ErrEmptyResponse
		}
		if err != nil {
			return nil, err
		}
	} else {
		ips = append(ips, destination.Address.IP())
	}

	for i, ip := range ips {
		if i > 0 {
			if err == nil {
				break
			} else {
				logrus.Warn("dial system failed: ", err)
			}
			logrus.Debug("trying next address: ", ip.String())
		}
		destination.Address = v2rayNet.IPAddress(ip)
		conn, err = dialer.dial(ctx, source, destination, sockopt)
	}

	return conn, err
}

func (dialer protectedDialer) dial(ctx context.Context, source v2rayNet.Address, destination v2rayNet.Destination, sockopt *internet.SocketConfig) (conn net.Conn, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	destIp := destination.Address.IP()
	ipv6 := len(destIp) != net.IPv4len
	fd, err := getFd(destination.Network, ipv6)
	if err != nil {
		return nil, err
	}

	if !dialer.protector.Protect(int32(fd)) {
		return nil, errors.New("protect failed")
	}

	if sockopt != nil {
		internet.ApplySockopt(sockopt, destination, uintptr(fd), ctx)
	}

	var sockaddr unix.Sockaddr
	if !ipv6 {
		socketAddress := &unix.SockaddrInet4{
			Port: int(destination.Port),
		}
		copy(socketAddress.Addr[:], destIp)
		sockaddr = socketAddress
	} else {
		socketAddress := &unix.SockaddrInet6{
			Port: int(destination.Port),
		}
		copy(socketAddress.Addr[:], destIp)
		sockaddr = socketAddress
	}

	err = unix.Connect(fd, sockaddr)
	if err != nil {
		return nil, err
	}

	file := os.NewFile(uintptr(fd), "socket")
	if file == nil {
		return nil, errors.New("failed to connect to fd")
	}

	switch destination.Network {
	case v2rayNet.Network_UDP:
		pc, err := net.FilePacketConn(file)
		if err == nil {
			destAddr, err := net.ResolveUDPAddr("udp", destination.NetAddr())
			if err != nil {
				return nil, err
			}
			conn = &internet.PacketConnWrapper{
				Conn: pc,
				Dest: destAddr,
			}
		}
	default:
		conn, err = net.FileConn(file)
	}

	if err != nil {
		return nil, err
	}

	comm.CloseIgnore(file)
	return conn, nil
}

func getFd(network v2rayNet.Network, ipv6 bool) (fd int, err error) {
	var af int
	if !ipv6 {
		af = unix.AF_INET
	} else {
		af = unix.AF_INET6
	}
	switch network {
	case v2rayNet.Network_TCP:
		fd, err = unix.Socket(af, unix.SOCK_STREAM, unix.IPPROTO_TCP)
	case v2rayNet.Network_UDP:
		fd, err = unix.Socket(af, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
	case v2rayNet.Network_UNIX:
		fd, err = unix.Socket(af, unix.SOCK_STREAM, 0)
	default:
		err = fmt.Errorf("unknow network")
	}
	return
}
