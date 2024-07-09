package bfd

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

type BFD struct {
	query chan query
	done  chan bool
}

type query struct {
	ip netip.Addr
	up chan bool
}

func (b *BFD) IsUp(addr netip.Addr) bool {
	q := query{ip: addr, up: make(chan bool)}
	b.query <- q
	return <-q.up
}

func (b *BFD) Start() error {

	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: 3784})

	if err != nil {
		return err
	}

	type msg struct {
		bfd  ControlPacket
		addr netip.Addr
	}

	recv := make(chan msg, 1000)
	b.done = make(chan bool)
	b.query = make(chan query, 1000)

	go func() {

		defer conn.Close()

		ipv4.NewPacketConn(conn).SetControlMessage(ipv4.FlagTTL, true)
		ipv6.NewPacketConn(conn).SetControlMessage(ipv6.FlagHopLimit, true)

		var cm ipv4.ControlMessage
		var v6 ipv6.ControlMessage
		var oob [128]byte

		for {
			select {
			case <-b.done:
				return
			default:
			}

			var buff [1500]byte

			// ReadMsgUDPAddrPort would be better?
			n, oobn, _, addr, err := conn.ReadMsgUDP(buff[:], oob[:])

			if err == nil && n >= 24 {

				ip, _ := netip.AddrFromSlice(addr.IP)

				if addr.IP.To4() == nil {
					// presumably IPv6 ...
					if v6.Parse(oob[:oobn]) == nil && v6.HopLimit == 255 {
						select {
						case recv <- msg{bfd: ControlPacket(buff[:n]), addr: ip}:
						default: // drop messages if the session receive queue is full
						}
					}
				} else {
					i := addr.IP.To4()
					ip = netip.AddrFrom4([4]byte{i[0], i[1], i[2], i[3]})
					if cm.Parse(oob[:oobn]) == nil && cm.TTL == 255 {
						select {
						case recv <- msg{bfd: ControlPacket(buff[:n]), addr: ip}:
						default: // drop messages if the session receive queue is full
						}
					}
				}
			}
		}
	}()

	go func() {

		type session struct {
			bfd chan ControlPacket
			ctx context.Context
			up  *atomic.Bool
		}

		sessions := map[netip.Addr]session{}

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		defer func() {
			for _, c := range sessions {
				close(c.bfd)
			}
		}()

		for {
			select {
			case <-b.done:
				return
			case q := <-b.query:
				if s, ok := sessions[q.ip]; ok {
					q.up <- s.up.Load()
				} else {
					q.up <- ok
				}
			case <-ticker.C:
				for ip, c := range sessions {
					select {
					case <-c.ctx.Done():
						close(c.bfd)
						delete(sessions, ip)
					default:
					}
				}
			case b := <-recv:
				c, ok := sessions[b.addr]

				if !ok {
					var up atomic.Bool
					ctx, cancel := context.WithCancel(context.TODO())
					bfd, err := udpSession(b.addr, cancel, &up)
					if err != nil {
						break
					}
					c = session{bfd: bfd, ctx: ctx, up: &up}
					sessions[b.addr] = c
				}

				select {
				case c.bfd <- b.bfd:
				case <-c.ctx.Done():
					close(c.bfd)
					delete(sessions, b.addr)
				default: // drop message if the sessions queue is full
				}
			}
		}
	}()

	return nil
}

func dialUDP(addr netip.Addr) (chan ControlPacket, error) {

	var ip net.IP

	if addr.Is4() {
		i := addr.As4()
		ip = i[:]
	} else if addr.Is6() {
		i := addr.As16()
		ip = i[:]
	} else {
		return nil, fmt.Errorf("Unsupported IP version")
	}

	laddr := net.UDPAddr{
		Port: 49152 + int(rand.Int31n(16384)),
	}

	raddr := net.UDPAddr{
		Port: 3784,
		IP:   ip,
	}

	out, err := net.DialUDP("udp", &laddr, &raddr)

	if err != nil {
		return nil, err
	}

	if addr.Is4() {
		err = ipv4.NewConn(out).SetTTL(255)
	} else {
		err = ipv6.NewConn(out).SetHopLimit(255)
	}

	if err != nil {
		return nil, err
	}

	xmit := make(chan ControlPacket, 1000)

	go func() {
		defer out.Close()
		for b := range xmit {
			out.Write(b)
		}
	}()

	return xmit, nil
}

func udpSession(addr netip.Addr, cancel context.CancelFunc, up *atomic.Bool) (chan ControlPacket, error) {

	xmit, err := dialUDP(addr)

	if err != nil {
		return nil, err
	}

	recv := make(chan ControlPacket, 1000)

	go func() {
		loop(recv, xmit, rand.Uint32(), up)
		defer close(xmit)
		defer cancel()
	}()

	return recv, nil
}
