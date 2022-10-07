package main

import (
  "io"
  "fmt"
  "net"
  "sync"
  "time"
  "strconv"
  "errors"
  "net/http"
  "os"
  "encoding/hex"

  "github.com/go-kit/log"
  "github.com/go-kit/log/level"
  "github.com/prometheus/client_golang/prometheus"
  "github.com/prometheus/client_golang/prometheus/promhttp"
  "github.com/prometheus/common/promlog"
  "github.com/prometheus/common/promlog/flag"
  "github.com/prometheus/common/version"
  "gopkg.in/alecthomas/kingpin.v2"
  "github.com/mdlayher/ethernet"
  "github.com/mdlayher/raw"
)

const (
  namespace   = "homeplug"
  etherType   = 0x88E1
)

var (
  hpVersion        = [...]byte{0x00}
  nwInfoReq        = [...]byte{0xA0, 0x38}
  nwInfoCnf        = [...]byte{0xA0, 0x39}
  hpVendor         = [...]byte{0x00, 0xB0, 0x52}

  listeningAddress = kingpin.Flag("telemetry.address", "Address on which to expose metrics.").Default(":9702").String()
  metricsEndpoint  = kingpin.Flag("telemetry.endpoint", "Path under which to expose metrics.").Default("/metrics").String()
  interfaceName    = kingpin.Flag("interface", "Interface to search for Homeplug devices.").String()
  destAddress      = kingpin.Flag("destaddr", "Destination MAC address for Homeplug devices.").Default("00B052000001").HexBytes()

  logger log.Logger
)

type Exporter struct {
 iface   *net.Interface
 conn    *raw.Conn
 dest    net.HardwareAddr
 mutex   sync.Mutex

 txRate  *prometheus.Desc
 rxRate  *prometheus.Desc
 network *prometheus.Desc
}

func NewExporter(iface *net.Interface, conn *raw.Conn, dest net.HardwareAddr) *Exporter {
  return &Exporter{
    iface:  iface,
    conn:   conn,
    dest:   dest,
    txRate: prometheus.NewDesc(
      prometheus.BuildFQName(namespace, "station", "tx_rate_bytes"),
      "Average PHY Tx data rate",
      []string{"mac_address", "terminal_equipment_identifier"},
      nil),
    rxRate: prometheus.NewDesc(
      prometheus.BuildFQName(namespace, "station", "rx_rate_bytes"),
      "Average PHY Rx data rate",
      []string{"mac_address", "terminal_equipment_identifier"},
      nil),
    network: prometheus.NewDesc(
      prometheus.BuildFQName(namespace, "network", "id"),
      "Logical network information",
      []string{"network_identifier", "terminal_equipment_identifier", "coordinator_mac_address"},
      nil),
  }
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
  ch <- e.txRate
  ch <- e.rxRate
  ch <- e.network
}

func (e *Exporter) Collect (ch chan<- prometheus.Metric) {
  e.mutex.Lock()
  defer e.mutex.Unlock()
  err := e.collect(ch)
  if err != nil {
    level.Error(logger).Log("msg", "error scraping Homeplug", "err", err)
  }
}

func (e *Exporter) collect(ch chan<- prometheus.Metric) error {
  netinfos, err := get_homeplug_netinfo(e.iface, e.conn, e.dest)
  if err != nil {
    return err
  }

  for _, info := range netinfos {
    for _, network := range info.Networks {
      ch <- prometheus.MustNewConstMetric(e.network, prometheus.GaugeValue,
            float64(network.ShortID), hex.EncodeToString(network.NetworkID[:]), strconv.FormatInt(int64(network.TEI), 10), network.CCoAddress.String())
    }

    for _, station := range info.Stations {
      ch <- prometheus.MustNewConstMetric(e.txRate, prometheus.GaugeValue,
            float64(uint64(station.TxRate) * 1024 * 1024 / 8), station.Address.String(), strconv.FormatInt(int64(station.TEI), 10))
      ch <- prometheus.MustNewConstMetric(e.rxRate, prometheus.GaugeValue,
            float64(uint64(station.RxRate) * 1024 * 1024 / 8), station.Address.String(), strconv.FormatInt(int64(station.TEI), 10))
    }
  }
  return nil
}

type HomeplugNetworkInfo struct {
  Networks []HomeplugNetworkStatus
  Stations []HomeplugStationStatus
}

func (n *HomeplugNetworkInfo) UnmarshalBinary(b []byte) error {
  o := 0

  var num_networks = int(b[o])
  o++
  for i := 0; i < num_networks; i++ {
    var ns HomeplugNetworkStatus
    size, err := (&ns).UnmarshalBinary(b[o:])
    if err != nil {
      return err
    }
    n.Networks = append(n.Networks, ns)
    o += size
  }

  var num_stations = int(b[o])
  o++
  for i := 0; i < num_stations; i++ {
    var ss HomeplugStationStatus
    size, err := (&ss).UnmarshalBinary(b[o:])
    if err != nil {
      return err
    }
    n.Stations = append(n.Stations, ss)
    o += size
  }

  return nil
}

type HomeplugNetworkStatus struct {
  NetworkID  [7]byte
  ShortID    uint8
  TEI        uint8
  Role       uint8
  CCoAddress net.HardwareAddr
  CCoTEI     uint8
}

func (s *HomeplugNetworkStatus) UnmarshalBinary(b []byte) (int, error) {
  if len(b) < 17 {
    return 0, io.ErrUnexpectedEOF
  }
  copy(s.NetworkID[:], b[0:7])
  s.ShortID = b[7]
  s.TEI = b[8]
  s.Role = b[9]
  s.CCoAddress = b[10:16]
  s.CCoTEI = b[16]
  return 17, nil
}

type HomeplugStationStatus struct {
  Address        net.HardwareAddr
  TEI            uint8
  BridgedAddress net.HardwareAddr
  TxRate         uint8
  RxRate         uint8
}

func (s *HomeplugStationStatus) UnmarshalBinary(b []byte) (int, error) {
  if len(b) < 15 {
    return 0, io.ErrUnexpectedEOF
  }
  s.Address = b[0:6]
  s.TEI = b[6]
  s.BridgedAddress = b[7:13]
  s.TxRate = b[13]
  s.RxRate = b[14]
  return 15, nil
}

type HomeplugFrame struct {
  Version [1]byte
  MMEType [2]byte
  Vendor  [3]byte
  Payload []byte
}

func (h *HomeplugFrame) MarshalBinary() ([]byte, error) {
  b := make([]byte, h.length())
  _, err := h.read(b)
  return b, err
}

func (h *HomeplugFrame) read(b []byte) (int, error) {
  b[0] = h.Version[0]
  b[1] = h.MMEType[1]
  b[2] = h.MMEType[0]
  b[3] = h.Vendor[0]
  b[4] = h.Vendor[1]
  b[5] = h.Vendor[2]
  copy(b[6:], h.Payload[:])
  return len(b), nil
}

func (h *HomeplugFrame) length() int {
  return 6 + len(h.Payload)
}

func (h *HomeplugFrame) UnmarshalBinary(b []byte) error {
  if len(b) < 6 {
    return io.ErrUnexpectedEOF
  }

  bb := make([]byte, len(b) - 6)
  copy(bb[:], b[6:])

  h.Version[0] = b[0]
  h.MMEType[1] = b[1]
  h.MMEType[0] = b[2]
  h.Vendor[0] = b[3]
  h.Vendor[1] = b[4]
  h.Vendor[2] = b[5]
  h.Payload = bb
  return nil
}

func main() {
  promlogConfig := &promlog.Config{}

  flag.AddFlags(kingpin.CommandLine, promlogConfig)
  kingpin.Version(version.Print("homeplug_exporter"))
  kingpin.HelpFlag.Short('h')
  kingpin.Parse()

  logger = promlog.New(promlogConfig)

  level.Info(logger).Log("msg", "Starting homeplug_exporter", "version", version.Info())
  level.Info(logger).Log("msg", "Build context", "build_context", version.BuildContext())

  iface, err := get_interface_or_default(*interfaceName)
  if err != nil {
    level.Error(logger).Log("msg", "failed to get interface", "err", err)
    os.Exit(1)
  }

  conn, err := raw.ListenPacket(iface, etherType, nil)
  if err != nil {
    level.Error(logger).Log("msg", "failed to listen", "err", err)
    os.Exit(1)
  }

  dest := net.HardwareAddr((*destAddress)[0:6])

  exporter := NewExporter(iface, conn, dest)
  prometheus.MustRegister(exporter)
  prometheus.MustRegister(version.NewCollector("homeplug_exporter"))

  level.Info(logger).Log("msg", fmt.Sprintf("Collecting from MAC address %s via interface %s", dest.String(), iface.Name))
  level.Info(logger).Log("msg", fmt.Sprintf("Starting Server: %s", *listeningAddress))

  http.Handle(*metricsEndpoint, promhttp.Handler())
  http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
    _, _ = w.Write([]byte(`<html>
             <head><title>Homeplug Exporter</title></head>
             <body>
             <h1>Homeplug Exporter</h1>
             <p><a href='` + *metricsEndpoint + `'>Metrics</a></p>
             </body>
             </html>`))
  })

  if err := http.ListenAndServe(*listeningAddress, nil); err != nil {
    level.Error(logger).Log("msg", "failed to bind HTTP server", "err", err)
    os.Exit(1)
  }
}

func get_homeplug_netinfo(iface *net.Interface, conn *raw.Conn, dest net.HardwareAddr) ([]HomeplugNetworkInfo, error) {
  ni := make([]HomeplugNetworkInfo, 0)
  ch := make(chan HomeplugFrame, 1)
  go read_homeplug(iface, conn, ch)

  err := write_homeplug(iface, conn, dest)
  if err != nil{
    return nil, fmt.Errorf("write_homeplug failed: %v", err)
  }

ChanLoop:
  for {
    select {
    case h := <-ch:
      if h.MMEType == nwInfoCnf {
        var n HomeplugNetworkInfo
        err := (&n).UnmarshalBinary(h.Payload)
        if err != nil{
          level.Error(logger).Log("msg", "failed to unmarshal network info frame", "err", err)
        } else {
          ni = append(ni, n)
        }
      } else {
        level.Error(logger).Log("msg", fmt.Sprintf("got unhandled mmetype: %v", h.MMEType))
      }
    case <- time.After(time.Second):
      break ChanLoop
    }
  }

  return ni, nil
}

func write_homeplug(iface *net.Interface, conn *raw.Conn, dest net.HardwareAddr) error {
  h := &HomeplugFrame{
    Version: hpVersion,
    MMEType: nwInfoReq,
    Vendor:  hpVendor,
  }

  b, err := h.MarshalBinary()
  if err != nil {
    return fmt.Errorf("failed to marshal homeplug frame: %v", err)
  }

  f := &ethernet.Frame{
    Destination: dest,
    Source:      iface.HardwareAddr,
    EtherType:   etherType,
    Payload:     b,
  }

  a := &raw.Addr{
    HardwareAddr: dest,
  }

  b, err = f.MarshalBinary()
  if err != nil {
    return fmt.Errorf("failed to marshal ethernet frame: %v", err)
  }

  _, err = conn.WriteTo(b, a)
  if err != nil {
    return fmt.Errorf("failed to send message: %v", err)
  }

  return nil
}

func read_homeplug(iface *net.Interface, conn *raw.Conn, ch chan<- HomeplugFrame) {
    b := make([]byte, iface.MTU)

    for {
      conn.SetReadDeadline(time.Now().Add(time.Second))
      n, addr, err := conn.ReadFrom(b)
      if err != nil {
        level.Debug(logger).Log("msg", "failed to receive message", "err", err)
        break
      }

      var f ethernet.Frame
      err = (&f).UnmarshalBinary(b[:n])
      if err != nil {
        level.Error(logger).Log("msg", "failed to unmarshal ethernet frame", "err", err)
        continue
      }

      var h HomeplugFrame
      err = (&h).UnmarshalBinary(f.Payload)
      if err != nil {
        level.Error(logger).Log("msg", "failed to unmarshal homeplug frame", "err",  err)
        continue
      }

      level.Debug(logger).Log("msg", fmt.Sprintf("[%v] %+v", addr, h))
      ch <- h
    }
  }

func get_interface_or_default(name string) (*net.Interface, error) {
  if *interfaceName == "" {
    ifaces, err := net.Interfaces()
    if err != nil {
      return nil, err
    }
    for _, iface := range ifaces {
      if iface.Flags & net.FlagUp == 0 {
        continue
      }
      if iface.Flags & net.FlagLoopback != 0 {
        continue
      }
      return &iface, nil
    }
  } else {
    iface, err := net.InterfaceByName(*interfaceName)
    if err != nil {
      return nil, err
    }
    return iface, nil
  }
  return nil, &net.OpError{Op: "route", Net: "ip+net", Source: nil, Addr: nil, Err: errors.New("invalid network interface")}
}
