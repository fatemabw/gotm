// AF_Packet support for gotm

package main

import (
	"bufio"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/daemon"
	dumbno "github.com/ncsa/dumbno-client-go"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	"github.com/google/gopacket/afpacket"
	"golang.org/x/net/bpf"
	"github.com/google/gopacket/pcapgo"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	MAX_ETHERNET_MTU       = 9216
	MINIMUM_IP_PACKET_SIZE = 58
	OUTPUT_BUFFER_SIZE     = 16 * 1024 * 1024
)

var (
	metricsAddress string

	iface              string
	filter             string
	packetTimeInterval time.Duration
	flowTimeout        time.Duration
	flowByteCutoff     uint
	flowPacketCutoff   uint
	writeOutputPath    string
	writeCompressed    bool

	rotationInterval time.Duration

	dumnoEndpoint          string
	largeFlowSizeMegabytes uint
	dumbnoEnabled          bool
	
	fanoutGroup    int
)

//Metrics
var (
	labels = []string{
		// Which interface
		"interface",
		// Which worker
		"worker",
	}

	mActiveFlows = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gotm_active_flow_count",
			Help: "Current number of active flows",
		}, labels,
	)
	mExpired = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gotm_expired_flow_count",
			Help: "Current number of expired flows in the last packetTimeInterval",
		}, labels,
	)
	mExpiredDurTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gotm_expired_flow_duration_seconds_sum",
			Help: "Total time spent expiring flows",
		}, labels,
	)
	mBytes = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gotm_bytes_total",
			Help: "Number of bytes seen",
		}, labels,
	)
	mBytesOutput = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gotm_bytes_output_total",
			Help: "Number of bytes output after filtering",
		}, labels,
	)
	mPackets = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gotm_packet_count",
			Help: "Number of packets seen",
		}, labels,
	)
	mOutput = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gotm_packet_output_count",
			Help: "Number of packets output after filtering",
		}, labels,
	)
	mFlows = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gotm_flow_count",
			Help: "Number of flows seen",
		}, labels,
	)

	mFlowSize = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "gotm_flow_size_bytes",
			Help:    "Bytes per flow",
			Buckets: prometheus.ExponentialBuckets(1024, 4, 15),
		},
	)
	mFlowsFiltered = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gotm_flows_filtered",
			Help: "Number of flows filtered",
		}, []string{"status"},
	)

	// These should be gauges, but can't.. https://github.com/prometheus/client_golang/issues/309
	mReceived = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gotm_packet_nic_received",
			Help: "Number of packets received by NIC",
		}, labels,
	)
	mDropped = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gotm_packet_nic_dropped",
			Help: "Number of packets dropped by NIC",
		}, labels,
	)
	mIfDropped = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gotm_packet_nic_if_dropped",
			Help: "Number of packets dropped by NIC at the interface",
		}, labels,
	)
)

func init() {
	flag.StringVar(&metricsAddress, "metrics-address", ":8080", "The address to listen on for HTTP requests for /metrics.")
	flag.StringVar(&iface, "interface", "eth0", "Comma separated list of interfaces")
	flag.StringVar(&filter, "filter", "ip or ip6", "bpf filter")
	flag.DurationVar(&packetTimeInterval, "timeinterval", 5*time.Second, "Interval between cleanups")
	flag.DurationVar(&flowTimeout, "flowtimeout", 5*time.Second, "Flow inactivity timeout")
	flag.UintVar(&flowByteCutoff, "bytecutoff", 8192, "Cut off flows after this many bytes")
	flag.UintVar(&flowPacketCutoff, "packetcutoff", 100, "Cut off flows after this many packets")
	flag.StringVar(&writeOutputPath, "write", "out", "Output path is $writeOutputPath/yyyy/mm/dd/ts.pcap")
	flag.BoolVar(&writeCompressed, "compress", false, "gzip pcaps as they are written")
	flag.DurationVar(&rotationInterval, "rotationinterval", 300*time.Second, "Interval between pcap rotations")

	flag.UintVar(&largeFlowSizeMegabytes, "largeflowsize", 1024, "Large flow size in megabytes")
	flag.StringVar(&dumnoEndpoint, "dumbno", "", "Endpoint that dumbno is listening on, i.e. 127.0.0.1:9000")
	
	flag.IntVar(&fanoutGroup, "fanoutGroup", 42, "fanout group id")
	
	prometheus.MustRegister(mActiveFlows)
	prometheus.MustRegister(mExpired)
	prometheus.MustRegister(mExpiredDurTotal)
	prometheus.MustRegister(mPackets)
	prometheus.MustRegister(mOutput)
	prometheus.MustRegister(mBytes)
	prometheus.MustRegister(mBytesOutput)
	prometheus.MustRegister(mFlows)
	prometheus.MustRegister(mReceived)
	prometheus.MustRegister(mDropped)
	prometheus.MustRegister(mIfDropped)
	prometheus.MustRegister(mFlowSize)
	prometheus.MustRegister(mFlowsFiltered)
}

type trackedFlow struct {
	packets   uint
	bytecount uint
	last      time.Time
	//The size in bytes at which this flow will be logged
	logthresh uint
}

func (t trackedFlow) String() string {
	return fmt.Sprintf("packets=%d bytecount=%d last=%s", t.packets, t.bytecount, t.last)
}

type PcapFrame struct {
	ci   gopacket.CaptureInfo
	data []byte
}

type FiveTuple struct {
	proto         layers.IPProtocol
	networkFlow   gopacket.Flow
	transportFlow gopacket.Flow
}

func (f FiveTuple) String() string {
	src, dst := f.networkFlow.Endpoints()
	sport, dport := f.transportFlow.Endpoints()
	return fmt.Sprintf("proto=%s src=%s sport=%s dst=%s dport=%s", f.proto, src, sport, dst, dport)
}

type FilterRequest struct {
	ft FiveTuple
	ts time.Time
}

func mustAtoiWithDefault(s string, defaultValue int) int {
	if s == "" {
		return defaultValue
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		log.Fatal(err)
	}
	return i
}

type afpacketHandle struct {
	TPacket *afpacket.TPacket
}

// SetBPFFilter translates a BPF filter string into BPF RawInstruction and applies them.
func (h *afpacketHandle) SetBPFFilter(filter string, snaplen int) (err error) {
	pcapBPF, err := pcap.CompileBPFFilter(layers.LinkTypeEthernet, snaplen, filter)
	if err != nil {
		return err
	}
	bpfIns := []bpf.RawInstruction{}
	for _, ins := range pcapBPF {
		bpfIns2 := bpf.RawInstruction{
			Op: ins.Code,
			Jt: ins.Jt,
			Jf: ins.Jf,
			K:  ins.K,
		}
		bpfIns = append(bpfIns, bpfIns2)
	}
	if h.TPacket.SetBPF(bpfIns); err != nil {
		return err
	}
	return h.TPacket.SetBPF(bpfIns)
}

func newAfpacketHandle(device string, snaplen int, block_size int, num_blocks int, timeout time.Duration) (*afpacketHandle, error) {

	h := &afpacketHandle{}
	var err error

	if device == "any" {
		h.TPacket, err = afpacket.NewTPacket(
			afpacket.OptFrameSize(snaplen),
			afpacket.OptBlockSize(block_size),
			afpacket.OptNumBlocks(num_blocks),
			afpacket.OptPollTimeout(timeout),
			afpacket.SocketRaw,
			afpacket.TPacketVersion3)
	} else {
		h.TPacket, err = afpacket.NewTPacket(
			afpacket.OptInterface(device),
			afpacket.OptFrameSize(snaplen),
			afpacket.OptBlockSize(block_size),
			afpacket.OptNumBlocks(num_blocks),
			afpacket.OptPollTimeout(timeout),
			afpacket.SocketRaw,
			afpacket.TPacketVersion3)
	}
	return h, err
}

// ZeroCopyReadPacketData satisfies ZeroCopyPacketDataSource interface
func (h *afpacketHandle) ZeroCopyReadPacketData() (data []byte, ci gopacket.CaptureInfo, err error) {
	return h.TPacket.ZeroCopyReadPacketData()
}

// SocketStats prints received, dropped, queue-freeze packet stats.
func (h *afpacketHandle) SocketStats() (as afpacket.SocketStats, asv afpacket.SocketStatsV3, err error) {
	return h.TPacket.SocketStats()
}

// afpacketComputeSize computes the block_size and the num_blocks in such a way that the
// allocated mmap buffer is close to but smaller than target_size_mb.
// The restriction is that the block_size must be divisible by both the
// frame size and page size.
func afpacketComputeSize(targetSizeMb int, snaplen int, pageSize int) (
	frameSize int, blockSize int, numBlocks int, err error) {

	if snaplen < pageSize {
		frameSize = pageSize / (pageSize / snaplen)
	} else {
		frameSize = (snaplen/pageSize + 1) * pageSize
	}

	// 128 is the default from the gopacket library so just use that
	blockSize = frameSize * 128
	numBlocks = (targetSizeMb) / blockSize

	if numBlocks == 0 {
		return 0, 0, 0, fmt.Errorf("Interface buffersize is too small")
	}

	return frameSize, blockSize, numBlocks, nil
}

func (h *afpacketHandle) SetFanout(t afpacket.FanoutType, id uint16) error {
	return h.TPacket.SetFanout(t, id)
}

// Close will close afpacket source.
func (h *afpacketHandle) Close() {
	h.TPacket.Close()
}

func doSniff(intf string, worker int, writerchan chan PcapFrame, flowfilterchan chan FilterRequest) {
	runtime.LockOSThread()
	log.Printf("Starting worker %d on interface %s", worker, intf)
	workerString := fmt.Sprintf("%d", worker)

	var err error

	//handle, err := pcap.OpenLive(intf, MAX_ETHERNET_MTU, true, pcap.BlockForever)
	//handle, err := afpacket.NewTPacket(
	//			afpacket.OptInterface(intf), 
	//			afpacket.OptFrameSize(MAX_ETHERNET_MTU), 
	//			afpacket.OptPollTimeout(pcap.BlockForever))

	szFrame, szBlock, numBlocks, err := afpacketComputeSize(OUTPUT_BUFFER_SIZE, MAX_ETHERNET_MTU, os.Getpagesize())
	if err != nil {
		panic(err)
	}
	afpacketHandle, err := newAfpacketHandle(intf, szFrame, szBlock, numBlocks, pcap.BlockForever)
	if err != nil {
		panic(err)
	}
	//err = handle.SetBPFFilter(filter)
	err = afpacketHandle.SetBPFFilter(filter, MAX_ETHERNET_MTU)
	if err != nil { // optional
		panic(err)
	}
	
	err = afpacketHandle.SetFanout(afpacket.FanoutHashWithDefrag, uint16(fanoutGroup))
	if err != nil {
		panic(err)
	}
	defer afpacketHandle.Close()
	
	seen := make(map[FiveTuple]*trackedFlow)
	var totalFlows, totalBytes, outputBytes, totalPackets, outputPackets uint

	lastcleanup := time.Now()

	var eth layers.Ethernet
	var dot1q layers.Dot1Q
	var ip4 layers.IPv4
	var ip6 layers.IPv6
	var tcp layers.TCP
	var udp layers.UDP
	parser := gopacket.NewDecodingLayerParser(layers.LayerTypeEthernet, &eth, &dot1q, &ip4, &ip6, &tcp, &udp)
	parser.IgnoreUnsupported = true
	decoded := []gopacket.LayerType{}
	var speedup int
	defaultLogThreshold := 1024 * 1024 * largeFlowSizeMegabytes
	for {
		packetData, ci, err := afpacketHandle.ZeroCopyReadPacketData()
		if err == io.EOF {
			break
		} else if err != nil {
			log.Fatal(err)
		}
		totalPackets += 1
		totalBytes += uint(len(packetData))

		err = parser.DecodeLayers(packetData, &decoded)
		var flow FiveTuple
		for _, layerType := range decoded {
			switch layerType {
			case layers.LayerTypeIPv6:
				flow.proto = ip6.NextHeader
				flow.networkFlow = ip6.NetworkFlow()
			case layers.LayerTypeIPv4:
				flow.proto = ip4.Protocol
				flow.networkFlow = ip4.NetworkFlow()
				//log.Println(worker, ip4.SrcIP, ip4.DstIP)
			case layers.LayerTypeUDP:
				flow.transportFlow = udp.TransportFlow()
			case layers.LayerTypeTCP:
				flow.transportFlow = tcp.TransportFlow()
			}
		}

		flw := seen[flow]
		if flw == nil {
			flw = &trackedFlow{logthresh: defaultLogThreshold}
			seen[flow] = flw
			//log.Println("NEW", flw, flow)
			totalFlows += 1
		}
		flw.last = time.Now()
		flw.packets += 1
		pl := uint(len(packetData))
		if pl > MINIMUM_IP_PACKET_SIZE {
			flw.bytecount += pl - MINIMUM_IP_PACKET_SIZE
		}
		if flw.bytecount < flowByteCutoff && flw.packets < flowPacketCutoff {
			//log.Println(flow, flw, "continues")
			outputPackets += 1
			outputBytes += uint(len(packetData))

			packetDataCopy := make([]byte, len(packetData))
			copy(packetDataCopy, packetData)

			writerchan <- PcapFrame{ci, packetDataCopy}
		} else if flw.bytecount > flw.logthresh {
			log.Printf("Large flow: megabytes=%d %s", flw.logthresh/1024/1024, flow)
			if dumbnoEnabled {
				flowfilterchan <- FilterRequest{ft: flow, ts: time.Now()}
			}
			flw.logthresh *= 2
		}
		//Cleanup
		speedup++
		if speedup == 5000 {
			speedup = 0
			//pcapStats, err = handle.Stats()
			ss, ss3, err := afpacketHandle.SocketStats()
			
			if err != nil {
				log.Fatal(err)
			}
			if time.Since(lastcleanup) > packetTimeInterval {
				lastcleanup = time.Now()
				//seen = make(map[string]*trackedFlow)
				var removedFlows int
				for flow, flw := range seen {
					if lastcleanup.Sub(flw.last) > flowTimeout {
						removedFlows += 1
						mFlowSize.Observe(float64(flw.bytecount))
						delete(seen, flow)
					}
				}
				log.Printf("if=%s W=%02d flows=%d removed=%d bytes=%d pkts=%d output=%d outpct=%.1f stats{rec, dropped}: %d Statsv3{rec, dropped, queue-freeze}: %d",
					intf, worker, len(seen), removedFlows,
					totalBytes, totalPackets, outputPackets, 100*float64(outputPackets)/float64(totalPackets),
					ss, ss3)

				expireSeconds := float64(time.Since(lastcleanup).Seconds())
				mExpired.WithLabelValues(intf, workerString).Set(float64(removedFlows))
				mExpiredDurTotal.WithLabelValues(intf, workerString).Add(expireSeconds)
			}
			mActiveFlows.WithLabelValues(intf, workerString).Set(float64(len(seen)))

			mFlows.WithLabelValues(intf, workerString).Add(float64(totalFlows))
			totalFlows = 0

			mPackets.WithLabelValues(intf, workerString).Add(float64(totalPackets))
			totalPackets = 0

			mBytes.WithLabelValues(intf, workerString).Add(float64(totalBytes))
			totalBytes = 0

			mBytesOutput.WithLabelValues(intf, workerString).Add(float64(outputBytes))
			outputBytes = 0

			mOutput.WithLabelValues(intf, workerString).Add(float64(outputPackets))
			outputPackets = 0

			//mReceived.WithLabelValues(intf, workerString).Set(float64(tp_packets))
			//mDropped.WithLabelValues(intf, workerString).Set(float64(tp_drops))
			//mIfDropped.WithLabelValues(intf, workerString).Set(float64(ss3[2]))
		}
	}
}

type pcapWrapper interface {
	WritePacket(ci gopacket.CaptureInfo, data []byte) error
	Close() error
}

type regularPcapWrapper struct {
	w io.WriteCloser
	b *bufio.Writer
	*pcapgo.Writer
}

func (wrapper *regularPcapWrapper) Close() error {
	flusherr := wrapper.b.Flush()
	ferr := wrapper.w.Close()

	if flusherr != nil {
		return flusherr
	}
	if ferr != nil {
		return ferr
	}

	return nil
}

type gzippedPcapWrapper struct {
	w io.WriteCloser
	z *gzip.Writer
	b *bufio.Writer
	*pcapgo.Writer
}

func (wrapper *gzippedPcapWrapper) Close() error {
	gzerr := wrapper.z.Close()
	flusherr := wrapper.b.Flush()
	ferr := wrapper.w.Close()

	if gzerr != nil {
		return gzerr
	}
	if flusherr != nil {
		return flusherr
	}
	if ferr != nil {
		return ferr
	}

	return nil
}

func openPcap(baseFilename string) (pcapWrapper, error) {
	if writeCompressed {
		baseFilename = baseFilename + ".gz"
	}
	log.Printf("Opening new pcap file %s", baseFilename)
	outf, err := os.Create(baseFilename)
	if err != nil {
		return nil, err
	}
	buffered := bufio.NewWriterSize(outf, OUTPUT_BUFFER_SIZE)
	if writeCompressed {
		outgz := gzip.NewWriter(buffered)
		pcapWriter := pcapgo.NewWriter(outgz)
		pcapWriter.WriteFileHeader(65536, layers.LinkTypeEthernet) // new file, must do this.
		return &gzippedPcapWrapper{
			w:      outf,
			z:      outgz,
			b:      buffered,
			Writer: pcapWriter,
		}, nil
	} else {
		pcapWriter := pcapgo.NewWriter(buffered)
		pcapWriter.WriteFileHeader(65536, layers.LinkTypeEthernet) // new file, must do this.
		return &regularPcapWrapper{
			w:      outf,
			b:      buffered,
			Writer: pcapWriter,
		}, nil
	}
}

//renamePcap renames the 'current' file to
//writeOutputPath/yyy/mm/dd/yyyy-mm-ddThh-mm-ss.pcap.gz

func renamePcap(tempName, outputPath string) error {
	datePart := time.Now().Format("2006/01/02/2006-01-02T15-04-05.pcap")
	if writeCompressed {
		datePart = datePart + ".gz"
		tempName = tempName + ".gz"
	}

	newName := filepath.Join(outputPath, datePart)
	//Ensure the directori exists
	if err := os.MkdirAll(filepath.Dir(newName), 0777); err != nil {
		return err
	}
	err := os.Rename(tempName, newName)

	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		log.Printf("moved %s to %s", tempName, newName)
	}
	return nil
}

func metrics() {
	http.Handle("/metrics", promhttp.Handler())
	err := http.ListenAndServe(metricsAddress, nil)
	if err != nil {
		log.Print(err)
	}
	//Not fatal?
}

func filterFlows(reqs chan FilterRequest) {
	c, err := dumbno.NewClient(dumnoEndpoint)
	if err != nil {
		log.Fatal(err)
	}
	for req := range reqs {
		if time.Since(req.ts) > 60*time.Second {
			log.Printf("Ignoring flow filter request older than 60 seconds: %s", time.Since(req.ts))
			mFlowsFiltered.WithLabelValues("ignored").Add(1)
			continue
		}
		f := req.ft
		src, dst := f.networkFlow.Endpoints()
		sport, dport := f.transportFlow.Endpoints()

		var fr dumbno.FilterRequest

		if sport.String() != "[]" {
			sportInt := mustAtoiWithDefault(sport.String(), 0)
			dportInt := mustAtoiWithDefault(dport.String(), 0)
			fr = dumbno.FilterRequest{
				Proto: f.proto.String(),
				Src:   src.String(),
				Dst:   dst.String(),
				Sport: sportInt,
				Dport: dportInt,
			}
		} else {
			fr = dumbno.FilterRequest{
				Src: src.String(),
				Dst: dst.String(),
			}
		}
		err = c.AddACL(fr)
		if err != nil {
			log.Printf("filterFlows: AddACL Failed: %s", err)
			mFlowsFiltered.WithLabelValues("err").Add(1)
		} else {
			mFlowsFiltered.WithLabelValues("ok").Add(1)
		}
	}
}

func main() {
	flag.Parse()
	go metrics()

	flowFilterChan := make(chan FilterRequest, 10000)
	if dumnoEndpoint != "" {
		dumbnoEnabled = true
		go filterFlows(flowFilterChan)
	}

	currentFileName := fmt.Sprintf("%s_current.pcap.tmp", iface)
	workerCountString := os.Getenv("NUM_RINGS")
	workerCount := mustAtoiWithDefault(workerCountString, 1)

	pcapWriterChan := make(chan PcapFrame, 500000)

	interfaceList := strings.Split(iface, ",")

	for _, iface := range interfaceList {
		log.Printf("Starting capture on %s with %d workers", iface, workerCount)
		for worker := 0; worker < workerCount; worker++ {
			go doSniff(iface, worker, pcapWriterChan, flowFilterChan)
		}
	}

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	rotationTicker := time.NewTicker(rotationInterval)

	//Rename any leftover pcap files from a previous run
	renamePcap(currentFileName, writeOutputPath)

	var pcapWriter pcapWrapper
	pcapWriter, err := openPcap(currentFileName)
	if err != nil {
		log.Fatal("Error opening pcap", err)
	}

	daemon.SdNotify(false, "READY=1")
	for {
		select {
		case pcf := <-pcapWriterChan:
			err := pcapWriter.WritePacket(pcf.ci, pcf.data)
			if err != nil {
				pcapWriter.Close()
				log.Fatal("Error writing output pcap", err)
			}

		case <-rotationTicker.C:
			//FIXME: refactor/wrap the open/close/rename code?
			err = pcapWriter.Close()
			if err != nil {
				log.Fatal("Error closing pcap", err)
			}
			err = renamePcap(currentFileName, writeOutputPath)
			if err != nil {
				log.Fatal("Error renaming pcap", err)
			}
			pcapWriter, err = openPcap(currentFileName)
			if err != nil {
				log.Fatal("Error opening pcap", err)
			}

		case <-signals:
			log.Print("Control-C??")
			err = pcapWriter.Close()
			if err != nil {
				log.Fatal("Error Closing", err)
			}
			err = renamePcap(currentFileName, writeOutputPath)
			if err != nil {
				log.Fatal("Error renaming pcap", err)
			}
			os.Exit(0)
		}
	}
}
