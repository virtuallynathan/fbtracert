/**
 * Copyright (c) 2016-present, Facebook, Inc.
 * All rights reserved.
 *
 * This source code is licensed under the BSD-style license found in the
 * LICENSE file in the root directory of this source tree. An additional grant
 * of patent rights can be found in the PATENTS file in the same directory.
 */

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/olekukonko/tablewriter"
)

//
// Command line flags
//
var maxTTL = flag.Int("maxTTL", 30, "The maximum ttl to use")
var minTTL = flag.Int("minTTL", 1, "The ttl to start at")
var maxSrcPorts = flag.Int("maxSrcPorts", 256, "The maximum number of source ports to use")
var maxTime = flag.Int("maxTime", 60, "The time to run the process for")
var targetPort = flag.Int("targetPort", 22, "The target port to trace to")
var probeRate = flag.Int("probeRate", 96, "The probe rate per ttl layer")
var tosValue = flag.Int("tosValue", 140, "The TOS/TC to use in probes")
var numResolvers = flag.Int("numResolvers", 32, "The number of DNS resolver goroutines")
var addrFamily = flag.String("addrFamily", "ip4", "The address family (ip4/ip6) to use for testing")
var maxColumns = flag.Int("maxColumns", 4, "Maximum number of columns in report tables")
var showAll = flag.Bool("showAll", false, "Show all paths, regardless of loss detection")
var srcAddr = flag.String("srcAddr", "", "The source address for pings, default to auto-discover")
var jsonOutput = flag.Bool("jsonOutput", false, "Output raw JSON data")
var baseSrcPort = flag.Int("baseSrcPort", 32768, "The base source port to start probing from")

// getSourceAddr discovers the source address for pinging
func getSourceAddr(af string, srcAddr string) (*net.IP, error) {

	if srcAddr != "" {
		addr, err := net.ResolveIPAddr(*addrFamily, srcAddr)
		if err != nil {
			return nil, err
		}
		return &addr.IP, nil
	}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if (ipnet.IP.To4() != nil && af == "ip4") || (ipnet.IP.To4() == nil && af == "ip6") {
				return &ipnet.IP, nil
			}
		}
	}
	return nil, fmt.Errorf("Could not find a source address in af %s", af)
}

// Resolve given hostname/address in the given address family
func resolveName(dest string, af string) (*net.IP, error) {
	addr, err := net.ResolveIPAddr(af, dest)
	return &addr.IP, err
}

// Probe is emitted by sender
type Probe struct {
	srcPort int
	ttl     int
}

// ICMPResponse is emitted by ICMPReceiver
type ICMPResponse struct {
	Probe
	fromAddr *net.IP
	fromName string
	rtt      uint32
}

// TCPResponse is emitted by TCPReceiver
type TCPResponse struct {
	Probe
	rtt uint32
}

// TCPReceiver Feeds on TCP RST messages we receive from the end host; we use lots of parameters to check if the incoming packet
// is actually a response to our probe. We create TCPResponse structs and emit them on the output channel
func TCPReceiver(done <-chan struct{}, af string, targetAddr string, probePortStart, probePortEnd, targetPort, maxTTL int) (chan interface{}, error) {
	var recvSocket int
	var err error
	var ipHdrSize int

	glog.V(2).Infoln("TCPReceiver starting...")

	// create the socket
	switch {
	case af == "ip4":
		recvSocket, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_TCP)
		ipHdrSize = 20 // IPv4 header is always included with the ipv4 raw socket receive
	case af == "ip6":
		recvSocket, err = syscall.Socket(syscall.AF_INET6, syscall.SOCK_RAW, syscall.IPPROTO_TCP)
		ipHdrSize = 0 // no IPv6 header present on TCP packets received on the raw socket

	default:
		return nil, fmt.Errorf("Unknown address family supplied")
	}

	if err != nil {
		return nil, err
	}

	// we'll be writing the TCPResponse structs to this channel
	out := make(chan interface{})

	// IP + TCP header, this channel is fed from the socket
	recv := make(chan TCPResponse)
	go func() {
		const tcpHdrSize int = 20
		packet := make([]byte, ipHdrSize+tcpHdrSize)

		for {
			n, from, err := syscall.Recvfrom(recvSocket, packet, 0)
			// parent has closed the socket likely
			if err != nil {
				break
			}

			// IP + TCP header size
			if n < ipHdrSize+tcpHdrSize {
				continue
			}

			// is that from the target port we expect?
			tcpHdr := parseTCPHeader(packet[ipHdrSize:n])
			if int(tcpHdr.Source) != targetPort {
				continue
			}

			// is that TCP RST or TCP ACK?
			if tcpHdr.Flags&RST != RST && tcpHdr.Flags&ACK != ACK {
				continue
			}

			var fromAddrStr string

			switch {
			case af == "ip4":
				fromAddrStr = net.IP((from.(*syscall.SockaddrInet4).Addr)[:]).String()
			case af == "ip6":
				fromAddrStr = net.IP((from.(*syscall.SockaddrInet6).Addr)[:]).String()
			}

			// is that from our target?
			if fromAddrStr != targetAddr {
				continue
			}

			// we extract the original TTL and timestamp from the ack number
			ackNum := tcpHdr.AckNum - 1
			ttl := int(ackNum >> 24)

			if ttl > maxTTL || ttl < 1 {
				continue
			}

			// recover the time-stamp from the ack #
			ts := ackNum & 0x00ffffff
			now := uint32(time.Now().UnixNano()/(1000*1000)) & 0x00ffffff

			// received timestamp is higher than local time; it is possible
			// that ts == now, since our clock resolution is coarse
			if ts > now {
				continue
			}

			recv <- TCPResponse{Probe: Probe{srcPort: int(tcpHdr.Destination), ttl: ttl}, rtt: now - ts}
		}
	}()

	go func() {
		defer syscall.Close(recvSocket)
		defer close(out)
		for {
			select {
			case response := <-recv:
				out <- response
			case <-done:
				glog.V(2).Infoln("TCPReceiver terminating...")
				return
			}
		}
	}()

	return out, nil
}

// ICMPReceiver runs on its own collecting Icmp responses until its explicitly told to stop
func ICMPReceiver(done <-chan struct{}, af string) (chan interface{}, error) {
	var recvSocket int
	var err error
	var outerIPHdrSize int
	var innerIPHdrSize int
	var icmpMsgType byte

	const (
		icmpHdrSize int = 8
		tcpHdrSize  int = 8
	)

	switch {
	case af == "ip4":
		recvSocket, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_ICMP)
		outerIPHdrSize = 20 // IPv4 raw socket always prepends the transport IPv4 header
		innerIPHdrSize = 20 // size of IPv4 header of the original TCP packet we used in the probes
		icmpMsgType = 11    // hardcoded: time to live exceeded

	case af == "ip6":
		recvSocket, err = syscall.Socket(syscall.AF_INET6, syscall.SOCK_RAW, syscall.IPPROTO_ICMPV6)
		outerIPHdrSize = 0  // IPv6 raw socket does not prepend the original transport IPv6 header
		innerIPHdrSize = 40 // size of IPv6 header of the original TCP packet we used in the probes
		icmpMsgType = 3     // time to live exceeded

	}

	if err != nil {
		return nil, err
	}

	glog.V(2).Infoln("ICMPReceiver is starting...")

	recv := make(chan interface{})

	go func() {
		// TODO: remove hardcode; 20 bytes for IP header, 8 bytes for ICMP header, 8 bytes for TCP header
		packet := make([]byte, outerIPHdrSize+icmpHdrSize+innerIPHdrSize+tcpHdrSize)
		for {
			n, from, err := syscall.Recvfrom(recvSocket, packet, 0)
			if err != nil {
				break
			}
			// extract the 8 bytes of the original TCP header
			if n < outerIPHdrSize+icmpHdrSize+innerIPHdrSize+tcpHdrSize {
				continue
			}
			// not ttl exceeded
			if packet[outerIPHdrSize] != icmpMsgType || packet[outerIPHdrSize+1] != 0 {
				continue
			}
			glog.V(4).Infof("Received icmp response message %d: %x\n", len(packet), packet)
			tcpHdr := parseTCPHeader(packet[outerIPHdrSize+icmpHdrSize+innerIPHdrSize : n])

			var fromAddr net.IP

			switch {
			case af == "ip4":
				fromAddr = net.IP(from.(*syscall.SockaddrInet4).Addr[:])
			case af == "ip6":
				fromAddr = net.IP(from.(*syscall.SockaddrInet6).Addr[:])
			}

			// extract ttl bits from the ISN
			ttl := int(tcpHdr.SeqNum) >> 24

			// extract the timestamp from the ISN
			ts := tcpHdr.SeqNum & 0x00ffffff
			// scale the current time
			now := uint32(time.Now().UnixNano()/(1000*1000)) & 0x00ffffff
			recv <- ICMPResponse{Probe: Probe{srcPort: int(tcpHdr.Source), ttl: ttl}, fromAddr: &fromAddr, rtt: now - ts}
		}
	}()

	out := make(chan interface{})
	go func() {
		defer syscall.Close(recvSocket)
		defer close(out)
		for {
			select {
			// read ICMP struct
			case response := <-recv:
				out <- response
			case <-done:
				glog.V(2).Infoln("ICMPReceiver done")
				return
			}
		}
	}()

	return out, nil
}

// Resolver resolves names in incoming ICMPResponse messages
// Everything else is passed through as is
func Resolver(input chan interface{}) (chan interface{}, error) {
	out := make(chan interface{})
	go func() {
		defer close(out)

		for val := range input {
			switch val.(type) {
			case ICMPResponse:
				resp := val.(ICMPResponse)
				names, err := net.LookupAddr(resp.fromAddr.String())
				if err != nil {
					resp.fromName = "?"
				} else {
					resp.fromName = names[0]
				}
				out <- resp
			default:
				out <- val
			}
		}
	}()
	return out, nil
}

// Sender generates TCP SYN packet probes with given TTL at given packet per second rate
// The packet descriptions are published to the output channel as Probe messages
// As a side effect, the packets are injected into raw socket
func Sender(done <-chan struct{}, srcAddr *net.IP, af, dest string, dstPort, baseSrcPort, maxSrcPorts, maxIters, ttl, pps, tos int) (chan interface{}, error) {
	var err error

	out := make(chan interface{})

	glog.V(2).Infof("Sender for ttl %d starting\n", ttl)

	dstAddr, err := resolveName(dest, af)
	if err != nil {
		return nil, err
	}

	var sendSocket int

	// create the socket
	switch {
	case af == "ip4":
		sendSocket, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_TCP)
	case af == "ip6":
		sendSocket, err = syscall.Socket(syscall.AF_INET6, syscall.SOCK_RAW, syscall.IPPROTO_TCP)
	}

	if err != nil {
		return nil, err
	}

	// bind the socket
	switch {
	case af == "ip4":
		var sockaddr [4]byte
		copy(sockaddr[:], srcAddr.To4())
		err = syscall.Bind(sendSocket, &syscall.SockaddrInet4{Port: 0, Addr: sockaddr})
	case af == "ip6":
		var sockaddr [16]byte
		copy(sockaddr[:], srcAddr.To16())
		err = syscall.Bind(sendSocket, &syscall.SockaddrInet6{Port: 0, Addr: sockaddr})
	}

	if err != nil {
		return nil, err
	}

	// set the ttl on the socket
	switch {
	case af == "ip4":
		err = syscall.SetsockoptInt(sendSocket, syscall.IPPROTO_IP, syscall.IP_TTL, ttl)
	case af == "ip6":
		err = syscall.SetsockoptInt(sendSocket, syscall.IPPROTO_IPV6, syscall.IPV6_UNICAST_HOPS, ttl)
	}

	if err != nil {
		return nil, err
	}

	// set the tos on the socket
	switch {
	case af == "ip4":
		err = syscall.SetsockoptInt(sendSocket, syscall.IPPROTO_IP, syscall.IP_TOS, tos)
	case af == "ip6":
		err = syscall.SetsockoptInt(sendSocket, syscall.IPPROTO_IPV6, syscall.IPV6_TCLASS, tos)
	}

	if err != nil {
		return nil, err
	}

	// spawn a new goroutine and return the channel to be used for reading
	go func() {
		defer syscall.Close(sendSocket)
		defer close(out)

		delay := time.Duration(1000/pps) * time.Millisecond

		for i := 0; i < maxSrcPorts*maxIters; i++ {
			srcPort := baseSrcPort + i%maxSrcPorts
			probe := Probe{srcPort: srcPort, ttl: ttl}
			now := uint32(time.Now().UnixNano()/(1000*1000)) & 0x00ffffff
			seqNum := ((uint32(ttl) & 0xff) << 24) | (now & 0x00ffffff)
			packet := makeTCPHeader(af, srcAddr, dstAddr, srcPort, dstPort, seqNum)

			switch {
			case af == "ip4":
				var sockaddr [4]byte
				copy(sockaddr[:], dstAddr.To4())
				err = syscall.Sendto(sendSocket, packet, 0, &syscall.SockaddrInet4{Port: 0, Addr: sockaddr})
			case af == "ip6":
				var sockaddr [16]byte
				copy(sockaddr[:], dstAddr.To16())
				// with IPv6 the dst port must be zero, otherwise the syscall fails
				err = syscall.Sendto(sendSocket, packet, 0, &syscall.SockaddrInet6{Port: 0, Addr: sockaddr})
			}

			if err != nil {
				glog.Errorf("Error sending packet %s\n", err)
				break
			}

			// grab time before blocking on send channel
			start := time.Now()
			select {
			case out <- probe:
				end := time.Now()
				jitter := time.Duration(((rand.Float64()-0.5)/20)*1000/float64(pps)) * time.Millisecond
				if end.Sub(start) < delay+jitter {
					time.Sleep(delay + jitter - (end.Sub(start)))
				}
			case <-done:
				glog.V(2).Infof("Sender for ttl %d exiting prematurely\n", ttl)
				return
			}
		}
		glog.V(2).Infoln("Sender done")
	}()

	return out, nil
}

// normalizeRcvd normalizes the rcvd results by send count to get the hit rate
func normalizeRcvd(sent, rcvd []int) ([]float64, error) {
	if len(rcvd) != len(sent) {
		return nil, fmt.Errorf("Length mismatch for sent/rcvd")
	}

	result := make([]float64, len(rcvd))
	for i := range sent {
		result[i] = float64(rcvd[i]) / float64(sent[i])
	}

	return result, nil
}

// isLossy detects a pattern where all samples after a sample [i] have lower hit rate than [i]
// this normally indicates a breaking point after [i]
func isLossy(hitRates []float64) bool {
	var found bool
	var segLen int
	for i := 0; i < len(hitRates)-1 && !found; i++ {
		found = true
		segLen = len(hitRates) - i
		for j := i + 1; j < len(hitRates); j++ {
			if hitRates[j] >= hitRates[i] {
				found = false
				break
			}
		}
	}
	// do not alarm on single-hop segment
	if segLen > 2 {
		return found
	}
	return false
}

// printLossyPaths prints the paths reported as having loss
func printLossyPaths(sent, rcvd map[int][]int, hops map[int][]string, maxColumns, maxTTL int) {
	var allPorts []int

	for srcPort := range hops {
		allPorts = append(allPorts, srcPort)
	}

	// split in multiple tables to fit the columns on the screen
	for i := 0; i < len(allPorts)/maxColumns; i++ {
		data := make([][]string, maxTTL)
		table := tablewriter.NewWriter(os.Stdout)
		header := []string{"TTL"}

		maxOffset := (i + 1) * maxColumns
		if maxOffset > len(allPorts) {
			maxOffset = len(allPorts)
		}

		for _, srcPort := range allPorts[i*maxColumns : maxOffset] {
			header = append(header, fmt.Sprintf("port: %d", srcPort), fmt.Sprintf("sent/rcvd"))
		}

		table.SetHeader(header)

		for ttl := 0; ttl < maxTTL-1; ttl++ {
			data[ttl] = make([]string, 2*(maxOffset-i*maxColumns)+1)
			data[ttl][0] = fmt.Sprintf("%d", ttl+1)
			for j, srcPort := range allPorts[i*maxColumns : maxOffset] {
				data[ttl][2*j+1] = hops[srcPort][ttl]
				data[ttl][2*j+2] = fmt.Sprintf("%02d/%02d", sent[srcPort][ttl], rcvd[srcPort][ttl])
			}
		}

		for _, v := range data {
			table.Append(v)
		}

		table.Render()
		fmt.Fprintf(os.Stdout, "\n")
	}
}

// Report defines a JSON report from go/fbtracert
type Report struct {
	// maps that store various counters per source port/ttl
	// e.g. sent, for every source port, contains vector of sent packets for each TTL
	Paths map[int][]string // The path map of srcPort(int) -> path hops ([]string)
	Sent  map[int][]int    // Probe count sent per source port/hop name
	Rcvd  map[int][]int    // Probe count received per source port/hop name

}

// newReport generates a new struct for our tracert data
func newReport() (report Report) {
	report.Paths = make(map[int][]string)
	report.Sent = make(map[int][]int)
	report.Rcvd = make(map[int][]int)

	return report
}

// printLossyPathsJSON prints raw JSON output for external program to analyze
func printLossyPathsJSON(sent, rcvd map[int][]int, hops map[int][]string, maxTTL int) {
	var report = newReport()

	for srcPort, path := range hops {
		report.Paths[srcPort] = path
		report.Sent[srcPort] = sent[srcPort]
		report.Rcvd[srcPort] = rcvd[srcPort]
	}

	b, err := json.MarshalIndent(report, "", "\t")
	if err != nil {
		glog.Errorf("Could not generate JSON %s", err)
		return
	}
	fmt.Fprintf(os.Stdout, "%s\n", b)
}

func main() {
	flag.Parse()
	if flag.Arg(0) == "" {
		fmt.Fprintf(os.Stderr, "Must specify a target\n")
		return
	}
	target := flag.Arg(0)

	var probes []chan interface{}

	numIters := int(*maxTime * *probeRate / *maxSrcPorts)

	if numIters <= 1 {
		fmt.Fprintf(os.Stderr, "Number of iterations too low, increase probe rate / run time or decrease src port range...\n")
		return
	}

	source, err := getSourceAddr(*addrFamily, *srcAddr)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not identify a source address to trace from\n")
		return
	}

	fmt.Fprintf(os.Stderr, "Starting fbtracert with %d probes per second/ttl, base src port %d and with the port span of %d\n", *probeRate, *baseSrcPort, *maxSrcPorts)
	fmt.Fprintf(os.Stderr, "Use '-logtostderr=true' cmd line option to see GLOG output\n")

	// this will catch senders quitting - we have one sender per ttl
	senderDone := make([]chan struct{}, *maxTTL)
	for ttl := *minTTL; ttl <= *maxTTL; ttl++ {
		senderDone[ttl-1] = make(chan struct{})
		c, err := Sender(senderDone[ttl-1], source, *addrFamily, target, *targetPort, *baseSrcPort, *maxSrcPorts, numIters, ttl, *probeRate, *tosValue)
		if err != nil {
			glog.Fatalf("Failed to start sender for ttl %d, %s\n -- are you running with the correct privileges?", ttl, err)
			return
		}
		probes = append(probes, c)
	}

	// channel to tell receivers to stop
	recvDone := make(chan struct{})

	// collect icmp unreachable messages for our probes
	icmpResp, err := ICMPReceiver(recvDone, *addrFamily)
	if err != nil {
		return
	}

	// collect TCP RST's from the target
	targetAddr, err := resolveName(target, *addrFamily)
	tcpResp, err := TCPReceiver(recvDone, *addrFamily, targetAddr.String(), *baseSrcPort, *baseSrcPort+*maxSrcPorts, *targetPort, *maxTTL)
	if err != nil {
		return
	}

	// add DNS name resolvers to the mix
	var resolved []chan interface{}
	unresolved := merge(tcpResp, icmpResp)

	for i := 0; i < *numResolvers; i++ {
		c, err := Resolver(unresolved)
		if err != nil {
			return
		}
		resolved = append(resolved, c)
	}

	counters := newReport()

	for srcPort := *baseSrcPort; srcPort < *baseSrcPort+*maxSrcPorts; srcPort++ {
		counters.Sent[srcPort] = make([]int, *maxTTL)
		counters.Rcvd[srcPort] = make([]int, *maxTTL)
		counters.Paths[srcPort] = make([]string, *maxTTL)
		//hops[srcPort][*maxTTL-1] = target

		for i := 0; i < *maxTTL; i++ {
			counters.Paths[srcPort][i] = "?"
		}
	}

	// collect all probe specs emitted by senders once all senders terminate, tell receivers to quit too
	go func() {
		for val := range merge(probes...) {
			probe := val.(Probe)
			counters.Sent[probe.srcPort][probe.ttl-1]++
		}
		glog.V(2).Infoln("All senders finished!")
		// give receivers time to catch up on in-flight data
		time.Sleep(2 * time.Second)
		// tell receivers to stop receiving
		close(recvDone)
	}()

	// this store DNS names of all nodes that ever replied to us
	var names []string

	// src ports that changed their paths in process of tracing
	var flappedPorts = make(map[int]bool)

	lastClosed := *maxTTL
	for val := range merge(resolved...) {
		switch val.(type) {
		case ICMPResponse:
			resp := val.(ICMPResponse)
			counters.Rcvd[resp.srcPort][resp.ttl-1]++
			currName := counters.Paths[resp.srcPort][resp.ttl-1]
			if currName != "?" && currName != resp.fromName {
				glog.V(2).Infof("%d: Source port %d flapped at ttl %d from: %s to %s\n", time.Now().UnixNano()/(1000*1000), resp.srcPort, resp.ttl, currName, resp.fromName)
				flappedPorts[resp.srcPort] = true
			}
			counters.Paths[resp.srcPort][resp.ttl-1] = resp.fromName
			// accumulate all names for processing later
			// XXX: we may have duplicates, which is OK,
			// but not very efficient
			names = append(names, resp.fromName)
		case TCPResponse:
			resp := val.(TCPResponse)
			// stop all senders sending above this ttl, since they are not needed
			// XXX: this is not always optimal, i.e. we may receive TCP RST for
			// a port mapped to a short WAN path, and it would tell us to terminate
			// probing at higher TTL, thus cutting visibility on "long" paths
			// however, this mostly concerned that last few hops...
			for i := resp.ttl; i < lastClosed; i++ {
				close(senderDone[i])
			}
			// update the last closed ttl, so we don't double-close the channels
			if resp.ttl < lastClosed {
				lastClosed = resp.ttl
			}
			counters.Rcvd[resp.srcPort][resp.ttl-1]++
			counters.Paths[resp.srcPort][resp.ttl-1] = target
		}
	}

	for srcPort, hopVector := range counters.Paths {
		for i := range hopVector {
			// truncate lists once we hit the target name
			if hopVector[i] == target && i < *maxTTL-1 {
				counters.Sent[srcPort] = counters.Sent[srcPort][:i+1]
				counters.Rcvd[srcPort] = counters.Rcvd[srcPort][:i+1]
				hopVector = hopVector[:i+1]
				break
			}
		}
	}

	if len(flappedPorts) > 0 {
		glog.Infof("A total of %d ports out of %d changed their paths while tracing\n", len(flappedPorts), *maxSrcPorts)
	}

	lossyCounters := newReport()

	// process the accumulated data, find and output lossy paths
	for port, sentVector := range counters.Sent {
		if flappedPorts[port] {
			continue
		}
		if rcvdVector, ok := counters.Rcvd[port]; ok {
			norm, err := normalizeRcvd(sentVector, rcvdVector)

			if err != nil {
				glog.Errorf("Could not normalize %v / %v", rcvdVector, sentVector)
				continue
			}

			if isLossy(norm) || *showAll {
				hosts := make([]string, len(norm))
				for i := range norm {
					hosts[i] = counters.Paths[port][i]
				}
				lossyCounters.Sent[port] = sentVector
				lossyCounters.Rcvd[port] = rcvdVector
				lossyCounters.Paths[port] = hosts
			}
		} else {
			glog.Errorf("No responses received for port %d", port)
		}
	}

	if len(lossyCounters.Paths) > 0 {
		if *jsonOutput {
			printLossyPathsJSON(lossyCounters.Sent, lossyCounters.Rcvd, lossyCounters.Paths, lastClosed+1)
		} else {
			printLossyPaths(lossyCounters.Sent, lossyCounters.Rcvd, lossyCounters.Paths, *maxColumns, lastClosed+1)
		}
		return
	}
	glog.Infof("Did not find any faulty paths\n")
}
