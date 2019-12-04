package main

import (
	"log"
	"net"
	"os"
	"strings"

	flags "github.com/jessevdk/go-flags"
	nknSdk "github.com/nknorg/nkn-sdk-go"
	"github.com/nknorg/nkn/common"
	"github.com/nknorg/tuna"
	ipify "github.com/rdegges/go-ipify"
	"github.com/trueinsider/smux"
)

var opts struct {
	BeneficiaryAddr string `short:"b" long:"beneficiary-addr" description:"Beneficiary address (NKN wallet address to receive rewards)"`
	ConfigFile      string `short:"c" long:"config" description:"Config file path" default:"config.entry.json"`
	ServicesFile    string `short:"s" long:"services" description:"Services file path" default:"services.json"`
	WalletFile      string `short:"w" long:"wallet" description:"Wallet file path" default:"wallet.json"`
	PasswordFile    string `short:"p" long:"password-file" description:"Wallet password file path" default:"wallet.pswd"`
}

func main() {
	_, err := flags.Parse(&opts)
	if err != nil {
		if flagsErr, ok := err.(*flags.Error); ok && flagsErr.Type == flags.ErrHelp {
			os.Exit(0)
		}
		log.Panicln(err)
	}

	nknSdk.Init()

	config := &tuna.EntryConfiguration{ReverseSubscriptionPrefix: tuna.DefaultSubscriptionPrefix}
	err = tuna.ReadJson(opts.ConfigFile, config)
	if err != nil {
		log.Panicln("Load config error:", err)
	}
	if len(opts.BeneficiaryAddr) > 0 {
		config.ReverseBeneficiaryAddr = opts.BeneficiaryAddr
	}

	if len(config.ReverseBeneficiaryAddr) > 0 {
		_, err = common.ToScriptHash(config.ReverseBeneficiaryAddr)
		if err != nil {
			log.Panicln("Invalid beneficiary address:", err)
		}
	}

	account, err := tuna.LoadOrCreateAccount(opts.WalletFile, opts.PasswordFile)
	if err != nil {
		log.Panicln("Load or create account error:", err)
	}

	wallet := nknSdk.NewWalletSDK(account)

	if config.Reverse {
		ip, err := ipify.GetIp()
		if err != nil {
			log.Panicln("Couldn't get IP:", err)
		}

		listener, err := net.ListenTCP(string(tuna.TCP), &net.TCPAddr{Port: config.ReverseTCP})
		if err != nil {
			log.Panicln("Couldn't bind listener:", err)
		}

		udpConn, err := net.ListenUDP(string(tuna.UDP), &net.UDPAddr{Port: config.ReverseUDP})
		if err != nil {
			log.Panicln("Couldn't bind listener:", err)
		}

		udpReadChans := make(map[string]chan []byte)
		udpCloseChan := make(chan struct{})

		go func() {
			for {
				buffer := make([]byte, 2048)
				n, addr, err := udpConn.ReadFromUDP(buffer)
				if err != nil {
					log.Println("Couldn't receive data from server:", err)
					if strings.Contains(err.Error(), "use of closed network connection") {
						udpCloseChan <- struct{}{}
						return
					}
					continue
				}

				data := make([]byte, n)
				copy(data, buffer)

				if udpReadChan, ok := udpReadChans[addr.String()]; ok {
					udpReadChan <- data
				}
			}
		}()

		go func() {
			for {
				tcpConn, err := listener.Accept()
				if err != nil {
					log.Println("Couldn't accept client connection:", err)
					tuna.Close(tcpConn)
					continue
				}

				te := tuna.NewTunaEntry(&tuna.Service{}, 0, 0, config, wallet)
				te.Session, _ = smux.Client(tcpConn, nil)
				stream, err := te.Session.OpenStream()
				if err != nil {
					log.Println("Couldn't open stream:", err)
					tuna.Close(tcpConn)
					continue
				}

				buf := make([]byte, 2048)
				n, err := stream.Read(buf)
				if err != nil {
					log.Println("Couldn't read service metadata:", err)
					tuna.Close(tcpConn)
					continue
				}
				metadataRaw := make([]byte, n)
				copy(metadataRaw, buf)

				te.SetMetadata(string(metadataRaw))

				te.SetServerTCPConn(tcpConn)

				if te.Metadata.UDPPort > 0 {
					ip, _, _ := net.SplitHostPort(tcpConn.RemoteAddr().String())
					udpAddr := net.UDPAddr{IP: net.ParseIP(ip), Port: te.Metadata.UDPPort}

					udpReadChan := make(chan []byte)
					udpWriteChan := make(chan []byte)

					go func() {
						for {
							select {
							case data := <-udpWriteChan:
								_, err := udpConn.WriteToUDP(data, &udpAddr)
								if err != nil {
									log.Println("Couldn't send data to server:", err)
								}
							case <-udpCloseChan:
								return
							}
						}
					}()

					udpReadChans[udpAddr.String()] = udpReadChan

					te.SetServerUDPReadChan(udpReadChan)
					te.SetServerUDPWriteChan(udpWriteChan)
				}
				go func() {
					te.StartReverse(stream)
					tuna.Close(tcpConn)
					te = nil
				}()
			}
		}()

		tuna.UpdateMetadata(
			tuna.DefaultReverseServiceName,
			255,
			[]int{},
			[]int{},
			ip,
			config.ReverseTCP,
			config.ReverseUDP,
			config.ReversePrice,
			config.ReverseBeneficiaryAddr,
			config.ReverseSubscriptionPrefix,
			config.ReverseSubscriptionDuration,
			config.ReverseSubscriptionFee,
			wallet,
		)
	} else {
		var services []tuna.Service
		err = tuna.ReadJson(opts.ServicesFile, &services)
		if err != nil {
			log.Panicln("Load service file error:", err)
		}

	service:
		for serviceName, serviceInfo := range config.Services {
			entryToExitMaxPrice, exitToEntryMaxPrice, err := tuna.ParsePrice(serviceInfo.MaxPrice)
			if err != nil {
				log.Panicln(err)
			}
			for _, service := range services {
				if service.Name == serviceName {
					go tuna.NewTunaEntry(&service, entryToExitMaxPrice, exitToEntryMaxPrice, config, wallet).Start()
					continue service
				}
			}
			log.Panicln("Service", serviceName, "not found in service file")
		}
	}

	select {}
}
