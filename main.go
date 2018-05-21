// PingViz: A handy application to visualize latency
// through a statsd interface

package main

import (
	"os"
	"os/signal"
	"fmt"
	"sync"
	"net"
	"strings"
	"time"
	"math/rand"
	log "github.com/Sirupsen/logrus"
	"github.com/spf13/viper"
	"gopkg.in/alexcesaro/statsd.v2"
	"golang.org/x/net/icmp"
    "golang.org/x/net/ipv4"
)

const ProtocolICMP = 1
const ListenAddr = "0.0.0.0"

var wg sync.WaitGroup
var hostChan chan checkHost
var stopChan chan os.Signal

type checkHost struct {
	name			string
	aRecord			*net.IPAddr
	ttl				time.Duration
	sleep			time.Duration
	successMetric 	string
	failedMetric 	string
}

// Config File Options
//
// ttl -> time.Duration (Default: '1s')
// : Duration before a packet is considered dropped.
//
// log -> string (Default: info)
// : Logging level
//
// sleep -> time.Duration (Default: '2s')
// : Duration between ping runs
//
// report.host -> string 
// : Statsd host to report to
//   
// report.prefix -> string
// : Prefix for reporting metrics into statsd
//
// report.postfix -> string
// : Postfix for reporting metrics into statsd
//
// report.normalize -> bool (Default: false)
// : Normalize hostnames / IPs being reported by turning periods into a non-special character (report.normalize_char)
//
// report.normalize_char -> string (Default: "_")
// : Character to substitute '.' for when report.naormalize equals True
//
// report.successful -> string (Default: "response.")
// : Statsd prefix for successful responses
//
// report.failed -> string (Default: "failed.")
// : Statsd prefix for dropped responses
//
// hosts.* -> map[string][]string
// : Group of hosts that belong together.  Group names get appended to the metrics hosts.
func readConfig(){
	viper.SetConfigName("pingviz") 
	viper.AddConfigPath("/etc/")
	viper.AddConfigPath("$HOME/.config/")
	viper.AddConfigPath(".")

	viper.SetDefault("ttl", "1s")
	viper.SetDefault("sleep", "2s")
	viper.SetDefault("log", "info")
	viper.SetDefault("report.normalize", false)
	viper.SetDefault("report.normalize_char", "_")
	viper.SetDefault("report.successful", "response.")
	viper.SetDefault("report.failed", "failed.")

	err := viper.ReadInConfig()
	if err != nil {
		fmt.Printf("Unable to load configuration file.")
		os.Exit(1)
	}
}


// Return a formatted string to report back to statsd
func metricName(host string, group string) (successful string, failed string) {
	if viper.GetBool("report.normalize") {
		host = strings.Replace(host, ".", viper.GetString("report.normalize_char"), -1)
	}

	successful = fmt.Sprintf(
		"%s%s%s.%s%s",
		viper.GetString("report.prefix"),
		viper.GetString("report.successful"),
		group,
		host,
		viper.GetString("report.postfix"),
	)
	failed = fmt.Sprintf(
		"%s%s%s.%s%s",
		viper.GetString("report.prefix"),
		viper.GetString("report.failed"),
		group,
		host, 
		viper.GetString("report.postfix"),
	)

	return
}

// Dispatcher running on a timer.
func (c checkHost) dispatch(hostchan chan <- checkHost, close <- chan os.Signal, wg *sync.WaitGroup) {
	log.WithFields(log.Fields{
		"host": c.name,
		"statsd_server": viper.GetString("report.host"),
	}).Debug("Starting Dispatcher")

	for {
		select {
		default:
			hostchan <- c
			time.Sleep(c.sleep)
		case <-close:
			log.Debug("Stopping dispatcher for ", c.name)
			wg.Done()
			return
		}
	}
}

func ping(host <- chan checkHost, wg *sync.WaitGroup) {
	log.Debug("Starting Pinger")

	statsdClient, err := statsd.New(statsd.Address(viper.GetString("report.host")))
	if err != nil {
		log.Fatal("Unable to connect to statsd host")
	} else {
		log.WithFields(log.Fields{
			"host": viper.GetString("report.host"),
		}).Debug("Connected to statsd")
	}
	defer statsdClient.Close()

	// Start Listening for icmp replies
	c, err := icmp.ListenPacket("ip4:icmp", ListenAddr)
    if err != nil {
        log.Fatal("Unable to listen for ICMP packets.  Are you running as root?")
    }
    defer c.Close()

    for h := range host{
		seq := rand.Intn(65535)

	    m := icmp.Message{
	        Type: ipv4.ICMPTypeEcho, Code: 0,
	        Body: &icmp.Echo{
	            ID: os.Getpid() & 0xffff, Seq: seq,
	            Data: []byte("pingviz"),
	        },
	    }
	    b, err := m.Marshal(nil)
	    if err != nil {
			log.WithFields(log.Fields{
				"host": h.name,
				"sequence": seq,
			}).Warn("Unable to create ICMP packet")
			continue
	    }

	    // Time we sent the packet
	    start := time.Now()

	    n, err := c.WriteTo(b, h.aRecord)
	    if err != nil {
	        log.WithFields(log.Fields{
	        	"host": h.name,
	        	"sequence": seq,
	        }).Error("Unable to send ICMP packet")
	        continue
	    } else if n != len(b) {
	        log.WithFields(log.Fields{
	        	"host": h.name,
	        	"sequence": seq,
	        }).Error("Packet Size mismatch")
	        continue
	    }

	    reply := make([]byte, 1500)
	    err = c.SetReadDeadline(time.Now().Add(h.ttl))
	    if err != nil {
	        log.Error("unable to set deadline")
	    }

	   	READ:
	    for {
	    	n, peer, err := c.ReadFrom(reply) 
	    	if err != nil {
					duration := time.Since(start)
					log.WithFields(log.Fields{
						"host": h.name,
						"duration": duration,
						"sequence": seq,
					}).Warn("Dropped Ping")
					statsdClient.Increment(h.failedMetric)
					break READ
	    	}

	    	if peer.String() == h.aRecord.String()  {
			    rm, err := icmp.ParseMessage(ProtocolICMP, reply[:n])
			    if err != nil {
			        log.Error("Unable to Parse ICMP Message")
			    }


				switch pkt := rm.Body.(type) {
				case *icmp.Echo:
					if pkt.Seq == seq {
						duration := time.Since(start)
						log.WithFields(log.Fields{
							"host": h.name,
							"duration": duration,
							"sequence": seq,
						}).Debug("Recived Ping")

						statsdClient.Timing(h.successMetric, duration.Seconds() * 1e3)
						break READ
					}
				default:
					log.Error("Unknown Error")
				}
	    	}
	    }

	}
	log.Debug("Closing Pinger")
}

func main() {
	readConfig()

	level, _ := log.ParseLevel(viper.GetString("log"))
	log.SetLevel(level)

	log.WithFields(log.Fields{
		"loglevel": level,
	}).Info("Starting up PingViz")


	hostChan = make(chan checkHost, 1)
	go ping(hostChan, &wg)

	for group := range viper.GetStringMapString("hosts") {
		config_group := fmt.Sprintf("hosts.%s", group)
		for _, host := range viper.GetStringSlice(config_group) {
			successMetric, failedMetric := metricName(host, group)


			// Resolve DNS or parse IP address
    		dst, err := net.ResolveIPAddr("ip4", host)
    		if err != nil {
    			log.WithFields(log.Fields{
					"host": host,
				}).Warn("Unable to resolve host; will not ping.")
    			continue
    		}

			log.WithFields(log.Fields{
				"group": group,
				"host": host,
				"a_record": dst,
				"success_metric": successMetric,
				"failed_metric": failedMetric,
			}).Debug("Found Host")

			c := checkHost{
				name: 			host,
				aRecord: 		dst,
				ttl:			viper.GetDuration("ttl"),
				sleep:			viper.GetDuration("sleep"),
				successMetric: 	successMetric,
				failedMetric: 	failedMetric,	
			}

			stopChan = make(chan os.Signal, 1)
			signal.Notify(stopChan, os.Interrupt)
			wg.Add(1)

			go c.dispatch(hostChan, stopChan, &wg)
		}
	}

	wg.Wait()
	close(hostChan)
}