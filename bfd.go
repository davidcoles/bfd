package bfd

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/netip"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const AdminDown = 0
const Down = 1
const Init = 2
const Up = 3

type BFD struct {
	query chan query
	done  chan bool
}

type query struct {
	ip netip.Addr
	up chan bool
}

func (b *BFD) Query(addr netip.Addr) bool {
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
		bfd  bfd
		addr netip.Addr
	}

	recv := make(chan msg, 1000)

	go func() {

		ipv4.NewPacketConn(conn).SetControlMessage(ipv4.FlagTTL, true)
		ipv6.NewPacketConn(conn).SetControlMessage(ipv6.FlagHopLimit, true)

		var cm ipv4.ControlMessage
		var v6 ipv6.ControlMessage
		var oob [128]byte

		for {
			var buff [1500]byte

			// ReadMsgUDPAddrPort would be better?
			n, oobn, _, addr, err := conn.ReadMsgUDP(buff[:], oob[:])

			if err == nil && n >= 24 {

				ip, _ := netip.AddrFromSlice(addr.IP)

				if addr.IP.To4() == nil {
					// presumably IPv6 ...
					if v6.Parse(oob[:oobn]) == nil && v6.HopLimit == 255 {
						select {
						case recv <- msg{bfd: bfd(buff[:n]), addr: ip}:
						default: // drop messages if the session receive queue is full
						}
					}
				} else {
					i := addr.IP.To4()
					ip = netip.AddrFrom4([4]byte{i[0], i[1], i[2], i[3]})
					if cm.Parse(oob[:oobn]) == nil && cm.TTL == 255 {
						select {
						case recv <- msg{bfd: bfd(buff[:n]), addr: ip}:
						default: // drop messages if the session receive queue is full
						}
					}
				}
			}
		}
	}()

	b.query = make(chan query, 1000)

	go func() {
		sessions := map[netip.Addr]session{}

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			select {
			case q := <-b.query:
				_, ok := sessions[q.ip]
				q.up <- ok
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
					c, err = startSession(b.addr)
					if err != nil {
						fmt.Println("session failed", b.addr, err)
						break
					}
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

func udp(addr netip.Addr) (chan bfd, error) {

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

	err = ipv4.NewConn(out).SetTTL(255)

	if err != nil {
		return nil, err
	}

	xmit := make(chan bfd, 1000)

	go func() {
		defer out.Close()
		//defer fmt.Println("closed", out)
		for b := range xmit {
			out.Write(b)
		}
	}()

	return xmit, nil
}

type session struct {
	bfd chan bfd
	ctx context.Context
}

func startSession(addr netip.Addr) (session, error) {
	xmit, err := udp(addr)
	if err != nil {
		return session{}, err
	}

	recv := make(chan bfd, 1000)
	ctx, cancel := context.WithCancel(context.TODO())

	go func() {
		var tx uint32 = 50 // ms
		var rx uint32 = 20 // ms

		defer cancel()
		defer close(xmit)

		var bfd stateVariables
		bfd.SessionState = Down
		bfd.LocalDiscr = rand.Uint32()
		bfd.DesiredMinTxInterval = tx * 1000  // convert to BFD timer unit (μs)
		bfd.RequiredMinRxInterval = rx * 1000 // convert to BFD timer unit (μs)
		bfd.DetectMult = 5
		bfd.LocalDiag = 1

		var echo bool
		var poll bool

		var interval uint32 = bfd.DesiredMinTxInterval
		timer := time.NewTimer(time.Duration(interval) * time.Microsecond)
		defer timer.Stop()

		detect := time.NewTimer(time.Duration(1000000) * time.Microsecond)
		detect.Stop()
		defer detect.Stop()

		var last []byte

		for {
			var t bool
			//t = true

			if t {
				switch bfd.SessionState {
				case AdminDown:
					fmt.Print("A")
				case Down:
					fmt.Print("D")
				case Init:
					fmt.Print("I")
				case Up:
					fmt.Print("U")
				}
			}

			select {
			case <-detect.C:
				bfd.SessionState = Down
				bfd.LocalDiag = 1
				last = nil
				fmt.Println("done")
				return

			case <-timer.C:
				// why did i do this here???
				if bfd.SessionState == Down {
					bfd.RemoteDiscr = 0
				}

				last = bfd.bfd(false, false)
				xmit <- last

				if interval > 0 {
					timer.Reset(time.Duration(interval) * time.Microsecond)
				}

			case b, ok := <-recv:
				if !ok {
					return
				}

				//fmt.Println(b)

				// 6.8.6

				// When a BFD Control packet is received, the following
				// procedure MUST be followed, in the order specified.  If
				// the packet is discarded according to these rules,
				// processing of the packet MUST cease at that point.

				// If the version number is not correct (1), the packet
				// MUST be discarded.
				if b.version() != 1 {
					continue
				}

				// If the Length field is less than the minimum correct
				// value (24 if the A bit is clear, or 26 if the A bit is
				// set), the packet MUST be discarded.
				if b.length() < 24 || (b.authentication() && b.length() < 26) {
					continue
				}

				// If the Length field is greater than the payload of the
				// encapsulating protocol, the packet MUST be discarded.

				// If the Detect Mult field is zero, the packet MUST be
				// discarded.
				if b.detectMult() == 0 {
					continue
				}

				// If the Multipoint (M) bit is nonzero, the packet MUST
				// be discarded.
				if b.multipoint() {
					continue
				}

				//If the My Discriminator field is zero, the packet MUST be
				//discarded.
				if b.myDiscriminator() == 0 {
					continue
				}

				//fmt.Println("RECV", foo(b))

				// TODO:
				// If the Your Discriminator field is nonzero, it MUST be used to
				// select the session with which this BFD packet is associated.  If
				// no session is found, the packet MUST be discarded.

				// If the Your Discriminator field is zero and the State
				// field is not Down or AdminDown, the packet MUST be
				// discarded.
				if b.yourDiscriminator() == 0 && (b.state() != Down && b.state() != AdminDown) {
					log.Println("b.yourDiscriminator ", b.state(), Down, AdminDown)
					continue
				}

				// If the Your Discriminator field is zero, the session MUST
				// be selected based on some combination of other fields,
				// possibly including source addressing information, the My
				// Discriminator field, and the interface over which the
				// packet was received.  The exact method of selection is
				// application specific and is thus outside the scope of this
				// specification.  If a matching session is not found, a new
				// session MAY be created, or the packet MAY be discarded.
				// This choice is outside the scope of this specification.

				// If the A bit is set and no authentication is in use
				// (bfd.AuthType is zero), the packet MUST be discarded.
				if b.authentication() {
					continue
				}

				// If the A bit is clear and authentication is in use
				// (bfd.AuthType is nonzero), the packet MUST be
				// discarded.
				if !b.authentication() && bfd.AuthType != 0 {
					continue
				}

				// If the A bit is set, the packet MUST be authenticated
				// under the rules of section 6.7, based on the
				// authentication type in use (bfd.AuthType).  This may
				// cause the packet to be discarded.
				if b.authentication() {
					continue
				}

				// Set bfd.RemoteDiscr to the value of My Discriminator.
				bfd.RemoteDiscr = b.myDiscriminator()

				// Set bfd.RemoteState to the value of the State (Sta) field.
				bfd.RemoteSessionState = b.state()

				// Set bfd.RemoteDemandMode to the value of the Demand (D) bit.
				bfd.RemoteDemandMode = b.demand()

				// Set bfd.RemoteMinRxInterval to the value of Required
				// Min RX Interval.
				bfd.RemoteMinRxInterval = b.requiredMinRxInterval()

				if b.requiredMinEchoRxInterval() == 0 {
					echo = false
				}

				if b.final() {
					poll = false
				}

				// Update the transmit interval as described in section 6.8.2.
				i := updateTransmitInterval(bfd, b.poll(), last)

				switch i {
				case 0:
					// stop sending control packets - drain channel
					if !timer.Stop() {
						<-timer.C
					}
					interval = 0
					//fmt.Print("0")
				case 1:
					// respond to a poll with final bit set
					last = bfd.bfd(false, true)
					xmit <- last
					//fmt.Print("1")
				case 2:
					last = bfd.bfd(false, true)
					xmit <- last
					//fmt.Print("2")
				default:
					// check if periodic packets are being sent - if not, start the timer
					if interval == 0 {
						if !timer.Stop() {
							<-timer.C
						}
						timer.Reset(time.Duration(i) * time.Microsecond)
					}
					interval = i // chenge the interval when the current timer expires
				}

				// Update the Detection Time as described in section 6.8.4.
				detectionTime := bfd.DesiredMinTxInterval
				if bfd.RemoteMinRxInterval > detectionTime {
					detectionTime = bfd.RemoteMinRxInterval
				}
				detectionTime *= uint32(bfd.DetectMult)
				detect.Reset(time.Duration(detectionTime) * time.Microsecond)

				if bfd.SessionState == AdminDown {
					panic("Discard AdminDown")
				}

				if b.state() == AdminDown {
					//If received state is AdminDown
					//  If bfd.SessionState is not Down
					//     Set bfd.LocalDiag to 3 (Neighbor signaled session down)
					//     Set bfd.SessionState to Down
					if bfd.SessionState != Down {
						bfd.LocalDiag = 3
						bfd.SessionState = Down
					}
				} else {
					//Else
					//    If bfd.SessionState is Down
					//        If received State is Down
					//            Set bfd.SessionState to Init
					//        Else if received State is Init
					//            Set bfd.SessionState to Up
					//    Else if bfd.SessionState is Init
					//        If received State is Init or Up
					//            Set bfd.SessionState to Up
					//    Else (bfd.SessionState is Up)
					//        If received State is Down
					//            Set bfd.LocalDiag to 3 (Neighbor signaled session down)
					//            Set bfd.SessionState to Down
					if bfd.SessionState == Down {
						if b.state() == Down {
							bfd.SessionState = Init
						} else if b.state() == Init {
							bfd.SessionState = Up
						}
					} else if bfd.SessionState == Init {
						if b.state() == Init || b.state() == Up {
							bfd.SessionState = Up
						}
					} else { // (bfd.SessionState is Up)
						if b.state() == Down {
							bfd.LocalDiag = 3
							bfd.SessionState = Down
						}
					}
				}

				// Check to see if Demand mode should become active or not (see section 6.6).
				checkDemandMode()

				// If bfd.RemoteDemandMode is 1, bfd.SessionState is Up, and
				// bfd.RemoteSessionState is Up, Demand mode is active on the
				// remote system and the local system MUST cease the periodic
				// transmission of BFD Control packets (see section 6.8.7).

				if bfd.RemoteDemandMode && bfd.SessionState == Up && bfd.RemoteSessionState == Up {
					interval = 0
					if !timer.Stop() {
						<-timer.C
					}
				}

				// If bfd.RemoteDemandMode is 0, or bfd.SessionState is not
				// Up, or bfd.RemoteSessionState is not Up, Demand mode is not
				// active on the remote system and the local system MUST send
				// periodic BFD Control packets (see section 6.8.7).
				if !bfd.RemoteDemandMode || bfd.SessionState != Up || bfd.RemoteSessionState != Up {
					if interval == 0 {
						interval = bfd.DesiredMinTxInterval
						if !timer.Stop() {
							<-timer.C
						}
						timer.Reset(time.Duration(interval) * time.Microsecond)
					}
				}

				// If the Poll (P) bit is set, send a BFD Control packet to
				// the remote system with the Poll (P) bit clear, and the
				// Final (F) bit set (see section 6.8.7).

				if b.poll() {
					// TODO: send packet with poll clear and final set
				}

				// If the packet was not discarded, it has been received for purposes
				// of the Detection Time expiration rules in section 6.8.4.

				if false {
					fmt.Println(bfd, echo, poll)
				}
			}
		}
	}()

	return session{bfd: recv, ctx: ctx}, nil
}

type stateVariables struct {
	SessionState          uint8
	RemoteSessionState    uint8
	LocalDiscr            uint32
	RemoteDiscr           uint32
	LocalDiag             uint8
	DesiredMinTxInterval  uint32
	RequiredMinRxInterval uint32
	RemoteMinRxInterval   uint32
	DemandMode            bool
	RemoteDemandMode      bool
	DetectMult            uint8
	AuthType              int
	//RcvAuthSeq
	//XmitAuthSeq
	//AuthSeqKnown
}

type bfd []byte

func (b bfd) version() uint8                    { return b[0] >> 5 }
func (b bfd) diag() uint8                       { return b[0] & 31 }
func (b bfd) state() uint8                      { return b[1] >> 6 }
func (b bfd) poll() bool                        { return b[1]&32 > 0 }
func (b bfd) final() bool                       { return b[1]&16 > 0 }
func (b bfd) cpi() bool                         { return b[1]&8 > 0 }
func (b bfd) authentication() bool              { return b[1]&4 > 0 }
func (b bfd) demand() bool                      { return b[1]&2 > 0 }
func (b bfd) multipoint() bool                  { return b[1]&1 > 0 }
func (b bfd) detectMult() uint8                 { return b[2] }
func (b bfd) length() uint8                     { return b[3] }
func (b bfd) myDiscriminator() uint32           { return ntohl([4]byte(b[4:8])) }
func (b bfd) yourDiscriminator() uint32         { return ntohl([4]byte(b[8:12])) }
func (b bfd) desiredMinTxInterval() uint32      { return ntohl([4]byte(b[12:16])) }
func (b bfd) requiredMinRxInterval() uint32     { return ntohl([4]byte(b[16:20])) }
func (b bfd) requiredMinEchoRxInterval() uint32 { return ntohl([4]byte(b[20:24])) }

//  0                   1                   2                   3
//  0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// |Vers |  Diag   |Sta|P|F|C|A|D|M|  Detect Mult  |    Length     |
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// |                       My Discriminator                        |
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// |                      Your Discriminator                       |
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// |                    Desired Min TX Interval                    |
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// |                   Required Min RX Interval                    |
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
// |                 Required Min Echo RX Interval                 |
// +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+

func (bfd stateVariables) bfd(poll, final bool) bfd {
	var r [24]byte

	r[0] = (byte(1) << 5) | bfd.LocalDiag
	r[1] |= bfd.SessionState << 6
	r[1] |= byte(ternary(poll, 32, 0))  // Poll (P)
	r[1] |= byte(ternary(final, 16, 0)) // Final (F)
	//r[1] |= 8 // Control Plane Independent (C)
	//r[1] |= 4 // Authentication Present (A)
	r[1] |= byte(ternary(bfd.DemandMode && bfd.SessionState == Up && bfd.RemoteSessionState == Up, 2, 0)) // Demand (D)
	// r[1] |= 1 // Multipoint (M) - Set to 0
	r[2] = bfd.DetectMult
	r[3] = byte(len(r))

	copy(r[4:8], htonls(bfd.LocalDiscr))
	copy(r[8:12], htonls(bfd.RemoteDiscr))
	copy(r[12:16], htonls(bfd.DesiredMinTxInterval))
	copy(r[16:20], htonls(bfd.RequiredMinRxInterval))
	copy(r[20:24], htonls(0)) // If this field is set to zero, the local system is unwilling or unable to loop back BFD Echo
	return r[:]
}

func updateTransmitInterval(bfd stateVariables, poll bool, last []byte) uint32 {

	// With the exceptions listed in the remainder of this section, a
	// system MUST NOT transmit BFD Control packets at an interval
	// less than the larger of bfd.DesiredMinTxInterval and
	// bfd.RemoteMinRxInterval, less applied jitter (see below). In
	// other words, the system reporting the slower rate determines
	// the transmission rate.

	interval := bfd.DesiredMinTxInterval
	if bfd.RemoteMinRxInterval > interval {
		interval = bfd.RemoteMinRxInterval
	}

	// The periodic transmission of BFD Control packets MUST be
	// jittered on a per-packet basis by up to 25%, that is, the
	// interval MUST be reduced by a random value of 0 to 25%, in
	// order to avoid self- synchronization with other systems on the
	// same subnetwork.  Thus, the average interval between packets
	// will be roughly 12.5% less than that negotiated.

	jitter := (interval * uint32(rand.Intn(25))) / 100 // 0-25% of rate

	// If bfd.DetectMult is equal to 1, the interval between
	// transmitted BFD Control packets MUST be no more than 90% of the
	// negotiated transmission interval, and MUST be no less than 75%
	// of the negotiated transmission interval.  This is to ensure
	// that, on the remote system, the calculated Detection Time does
	// not pass prior to the receipt of the next BFD Control packet.
	var min uint32 = 3 // 0, 1 and 2 are sentinal values - FIXME
	var max uint32 = interval
	if bfd.DetectMult != 0 {
		max = (interval * 90) / 100 // 90% of interval
		min = (interval * 75) / 100 // 75% of interval
	}

	// The transmit interval MUST be recalculated whenever
	// bfd.DesiredMinTxInterval changes, or whenever
	// bfd.RemoteMinRxInterval changes, and is equal to the greater of
	// those two values.  See sections 6.8.2 and 6.8.3 for details on
	// transmit timers.

	// IGNORE ABOVE FOR NOW - RECALCULATING EVERY TIME

	// A system MUST NOT transmit BFD Control packets if
	// bfd.RemoteDiscr is zero and the system is taking the Passive
	// role.
	if bfd.RemoteDiscr == 0 {
		return 0 // we are always passive
	}

	// A system MUST NOT periodically transmit BFD Control packets if
	// bfd.RemoteMinRxInterval is zero.
	if bfd.RemoteMinRxInterval == 0 {
		return 0
	}

	// A system MUST NOT periodically transmit BFD Control packets if
	// Demand mode is active on the remote system
	// (bfd.RemoteDemandMode is 1, bfd.SessionState is Up, and
	// bfd.RemoteSessionState is Up) and a Poll Sequence is not being
	// transmitted.
	if bfd.RemoteDemandMode && bfd.SessionState == Up && bfd.RemoteSessionState == Up && !poll {
		return 0
	}

	// If a BFD Control packet is received with the Poll (P) bit set
	// to 1, the receiving system MUST transmit a BFD Control packet
	// with the Poll (P) bit clear and the Final (F) bit set as soon
	// as practicable, without respect to the transmission timer or
	// any other transmission limitations, without respect to the
	// session state, and without respect to whether Demand mode is
	// active on either system.  A system MAY limit the rate at which
	// such packets are transmitted.  If rate limiting is in effect,
	// the advertised value of Desired Min TX Interval MUST be greater
	// than or equal to the interval between transmitted packets
	// imposed by the rate limiting function.

	if poll {
		return 1
	}

	// A system MUST NOT set the Demand (D) bit unless bfd.DemandMode
	// is 1, bfd.SessionState is Up, and bfd.RemoteSessionState is Up.

	// I thnk this is handled sufficiently in the constuction of the packet code
	//demand := false
	//if bfd.DemandMode && bfd.SessionState == Up && bfd.RemoteSessionState == Up {
	//	demand = true
	//}

	// A BFD Control packet SHOULD be transmitted during the interval
	// between periodic Control packet transmissions when the contents
	// of that packet would differ from that in the previously
	// transmitted packet (other than the Poll and Final bits) in
	// order to more rapidly communicate a change in state.
	if diff(last, bfd.bfd(false, false)) {
		return 2
	}

	interval -= jitter

	if interval < min {
		return min
	}

	if interval > max {
		return max
	}

	return interval
}

func ntohl(n [4]byte) uint32 {
	return uint32(n[0])<<24 |
		uint32(n[1])<<16 |
		uint32(n[2])<<8 |
		uint32(n[3])
}

func htonl(n uint32) (r [4]byte) {
	r[0] = byte(n >> 24)
	r[1] = byte(n >> 16)
	r[2] = byte(n >> 8)
	r[3] = byte(n)
	return
}

func ntohls(n []byte) uint32 { return ntohl([4]byte{n[0], n[1], n[2], n[3]}) }
func htonls(n uint32) []byte { r := htonl(n); return r[:] }

func ternary(c bool, t, f int) int {
	if c {
		return t
	}
	return f
}

func diff(a, b []byte) bool {
	var x, y [24]byte

	if len(a) < 24 || len(b) < 24 {
		return true
	}

	copy(x[:], a[:])
	copy(y[:], b[:])

	x[1] &= 0xcf // mask off poll+final
	y[1] &= 0xcf // mask off poll+final
	return x != y
}

func (b bfd) String() string {
	return fmt.Sprintf(
		"[v:%d d:%d s:%d p:%v f:%v c:%v a:%v d:%v m:%v dm:%d l:%d md:%d yd:%d tx:%d rx:%d e:%d]",
		b.version(),
		b.diag(),
		b.state(),
		b.poll(),
		b.final(),
		b.cpi(),
		b.authentication(),
		b.demand(),
		b.multipoint(),
		b.detectMult(),
		b.length(),
		b.myDiscriminator(),
		b.yourDiscriminator(),
		b.desiredMinTxInterval(),
		b.requiredMinRxInterval(),
		b.requiredMinEchoRxInterval(),
	)
}

func checkDemandMode() {
	// Demand mode is requested independently in each direction by
	// virtue of a system setting the Demand (D) bit in its BFD Control
	// packets.  The system receiving the Demand bit ceases the periodic
	// transmission of BFD Control packets.  If both systems are
	// operating in Demand mode, no periodic BFD Control packets will
	// flow in either direction.

	// Demand mode requires that some other mechanism is used to imply
	// continuing connectivity between the two systems.  The mechanism used
	// does not have to be the same in both directions, and is outside of
	// the scope of this specification.  One possible mechanism is the
	// receipt of traffic from the remote system; another is the use of the
	// Echo function.

	// When a system in Demand mode wishes to verify bidirectional
	// connectivity, it initiates a Poll Sequence (see section 6.5).  If no
	// response is received to a Poll, the Poll is repeated until the
	// Detection Time expires, at which point the session is declared to be
	// Down.  Note that if Demand mode is operating only on the local
	// system, the Poll Sequence is performed by simply setting the Poll (P)
	// bit in regular periodic BFD Control packets, as required by section
	// 6.5.

	// The Detection Time in Demand mode is calculated differently than in
	// Asynchronous mode; it is based on the transmit rate of the local
	// system, rather than the transmit rate of the remote system.  This
	// ensures that the Poll Sequence mechanism works properly.  See section
	// 6.8.4 for more details.

	// Note that the Poll mechanism will always fail unless the negotiated
	// Detection Time is greater than the round-trip time between the two
	// systems.  Enforcement of this constraint is outside the scope of this
	// specification.

	// Demand mode MAY be enabled or disabled at any time, independently in
	// each direction, by setting or clearing the Demand (D) bit in the BFD
	// Control packet, without affecting the BFD session state.  Note that
	// the Demand bit MUST NOT be set unless both systems perceive the
	// session to be Up (the local system thinks the session is Up, and the
	// remote system last reported Up state in the State (Sta) field of the
	// BFD Control packet).

	// When the transmitted value of the Demand (D) bit is to be changed,
	// the transmitting system MUST initiate a Poll Sequence in conjunction
	// with changing the bit in order to ensure that both systems are aware
	// of the change.

	// If Demand mode is active on either or both systems, a Poll Sequence
	// MUST be initiated whenever the contents of the next BFD Control
	// packet to be sent would be different than the contents of the
	// previous packet, with the exception of the Poll (P) and Final (F)
	// bits.  This ensures that parameter changes are transmitted to the
	// remote system and that the remote system acknowledges these changes.

	// Because the underlying detection mechanism is unspecified, and may
	// differ between the two systems, the overall Detection Time
	// characteristics of the path will not be fully known to either system.
	// The total Detection Time for a particular system is the sum of the
	// time prior to the initiation of the Poll Sequence, plus the
	// calculated Detection Time.

	// Note that if Demand mode is enabled in only one direction, continuous
	// bidirectional connectivity verification is lost (only connectivity in
	// the direction from the system in Demand mode to the other system will
	// be verified).  Resolving the issue of one system requesting Demand
	// mode while the other requires continuous bidirectional connectivity
	// verification is outside the scope of this specification.

}
