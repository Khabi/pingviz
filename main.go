package main

import (
	"os"
	"fmt"
	"sync"
	"strings"
	"os/signal"
	"math/rand"
	"github.com/spf13/viper"
	"time"
	"gopkg.in/alexcesaro/statsd.v2"
	log "github.com/Sirupsen/logrus"
    "net"
    "golang.org/x/net/icmp"
    "golang.org/x/net/ipv4"
)

var wg sync.WaitGroup
var stopChan chan os.Signal
var ListenAddr = "0.0.0.0"

const (
    ProtocolICMP = 1
)


type Host struct {
	SHost 				string 			// statsd host
	Server				string 			// Host to report on
	TTL					time.Duration	// Time before returning a timeout 
	Sleep				time.Duration	// Time between pings
	SuccesReportString 	string 			// statsd reporting name
	FailedReportString	string 			// statsd failed pings
}

func (h Host) Reporter(close <- chan os.Signal, wg *sync.WaitGroup) {
	log.Debug("Reporting stats for ", h.Server, " to ", h.SHost)
	c, err := statsd.New(statsd.Address(h.SHost))
	if err != nil {
		log.Error("Unable to connect to ", h.SHost)
		wg.Done()
		return
	}
	defer c.Close()

	for {
		select {
		default:
			dst, dur, err := Ping(h.Server)
			if err != nil {
				log.Debug("ERROR! ", h.Server)
				log.Debug(err)
				c.Increment(h.FailedReportString)
			} else {
				log.Info(dst, dur)
				c.Timing(h.SuccesReportString, dur.Seconds() * 1e3)

			}
			time.Sleep(h.Sleep)
		case <-close:
			wg.Done()
			return
		}
	}
}

func Ping(addr string) (*net.IPAddr, time.Duration, error) {
    // Start listening for icmp replies
    c, err := icmp.ListenPacket("ip4:icmp", ListenAddr)
    if err != nil {
        return nil, 0, err
    }
    defer c.Close()
    seq := rand.Intn(65535)

    // Resolve any DNS (if used) and get the real IP of the target
    dst, err := net.ResolveIPAddr("ip4", addr)
    if err != nil {
        panic(err)
        return nil, 0, err
    }

    // Make a new ICMP message
    m := icmp.Message{
        Type: ipv4.ICMPTypeEcho, Code: 0,
        Body: &icmp.Echo{
            ID: os.Getpid() & 0xffff, Seq: seq,
            Data: []byte(""),
        },
    }
    b, err := m.Marshal(nil)
    if err != nil {
        return dst, 0, err
    }


    // Send it
    start := time.Now()
    n, err := c.WriteTo(b, dst)
    if err != nil {
        return dst, 0, err
    } else if n != len(b) {
        return dst, 0, fmt.Errorf("got %v; want %v", n, len(b))
    }

    // Wait for a reply
    reply := make([]byte, 1500)
    err = c.SetReadDeadline(time.Now().Add(1 * time.Second))
    if err != nil {
        return dst, 0, err
    }
    
    for {
    	n, peer, err := c.ReadFrom(reply) 
    	if err != nil {
        	return dst, 0, err
    	}

    	if peer.String() == addr  {
		    // Pack it up boys, we're done here
		    rm, err := icmp.ParseMessage(ProtocolICMP, reply[:n])
		    if err != nil {
		        return dst, 0, err
		    }


			switch pkt := rm.Body.(type) {
			case *icmp.Echo:
				if pkt.Seq == seq {
					duration := time.Since(start)
					return dst, duration, nil
				}
			default:
				return dst, 0, fmt.Errorf("got %+v from %v; want echo reply", rm, peer)
			}
    	}
    }	

}



func main() {

	// Config Options
	viper.SetConfigName("gp") 
	viper.AddConfigPath("/etc/")
	viper.AddConfigPath("$HOME/.config/")
	viper.AddConfigPath(".")

	viper.SetDefault("ttl", "1s")
	viper.SetDefault("log", "info")
	viper.SetDefault("report.normalize", false)

	err := viper.ReadInConfig()
	if err != nil {
		log.Fatal("Fatal error config file: %s \n", err)
	}

	level, _ := log.ParseLevel(viper.GetString("log"))
	log.SetLevel(level)

	log.Info("Starting up PingViz")
	log.Info("Setting log level to ", level)

	statsd_prefix := viper.GetString("report.pre")
	statsd_postfix := viper.GetString("report.post")

	for group := range viper.GetStringMapString("hosts") {
		config_group := fmt.Sprintf("hosts.%s", group)
		for _, host := range viper.GetStringSlice(config_group) {
				var normal_host string
				if viper.GetBool("report.normalize") {
					normal_host = strings.Replace(host, ".", "_", -1)
				} else {
					normal_host = host
				}

				SuccessfulRS := fmt.Sprintf("%s%s%s.%s%s", statsd_prefix, viper.GetString("report.successful"), group, normal_host, statsd_postfix)
				FailedRS := fmt.Sprintf("%s%s%s.%s%s", statsd_prefix, viper.GetString("report.failed"), group, normal_host, statsd_postfix)

				h := Host{
					SHost: viper.GetString("report.host"),
					TTL: viper.GetDuration("ttl"),
					Sleep: viper.GetDuration("sleep"),
					SuccesReportString: SuccessfulRS,
					FailedReportString: FailedRS,
					Server: host,
				}
				wg.Add(1)

				stopChan = make(chan os.Signal, 1)
				signal.Notify(stopChan, os.Interrupt)

				go h.Reporter(stopChan, &wg)
		}
	}

	wg.Wait()
	log.Info(viper.GetStringSlice("hosts.internal"))
}