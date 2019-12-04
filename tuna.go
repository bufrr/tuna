package tuna

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	. "github.com/nknorg/nkn-sdk-go"
	"github.com/nknorg/nkn/common"
	"github.com/nknorg/nkn/crypto"
	"github.com/nknorg/nkn/crypto/util"
	"github.com/nknorg/nkn/program"
	"github.com/nknorg/nkn/util/address"
	"github.com/nknorg/nkn/util/config"
	"github.com/nknorg/nkn/vault"
)

type Protocol string

const (
	TCP                          Protocol = "tcp"
	UDP                          Protocol = "udp"
	DefaultNanoPayUpdateInterval          = time.Minute
	DefaultSubscriptionPrefix             = "tuna+1."
	DefaultReverseServiceName             = "reverse"
	TrafficUnit                           = 1024 * 1024
)

type Service struct {
	Name string `json:"name"`
	TCP  []int  `json:"tcp"`
	UDP  []int  `json:"udp"`
}

type Metadata struct {
	IP              string `json:"ip"`
	TCPPort         int    `json:"tcpPort"`
	UDPPort         int    `json:"udpPort"`
	ServiceId       byte   `json:"serviceId"`
	ServiceTCP      []int  `json:"serviceTcp,omitempty"`
	ServiceUDP      []int  `json:"serviceUdp,omitempty"`
	Price           string `json:"price,omitempty"`
	BeneficiaryAddr string `json:"beneficiaryAddr,omitempty"`
}

type Common struct {
	Service             *Service
	EntryToExitMaxPrice common.Fixed64
	ExitToEntryMaxPrice common.Fixed64
	Wallet              *WalletSDK
	DialTimeout         uint16
	SubscriptionPrefix  string
	Reverse             bool
	ReverseMetadata     *Metadata
	EntryToExitPrice    common.Fixed64
	ExitToEntryPrice    common.Fixed64
	PaymentReceiver     string
	Metadata            *Metadata

	connected    bool
	tcpConn      net.Conn
	udpConn      *net.UDPConn
	udpReadChan  chan []byte
	udpWriteChan chan []byte
	udpCloseChan chan struct{}
	tcpListener  *net.TCPListener
}

func (c *Common) SetServerTCPConn(conn net.Conn) {
	c.tcpConn = conn
}

func (c *Common) GetServerTCPConn(force bool) (net.Conn, error) {
	err := c.CreateServerConn(force)
	if err != nil {
		return nil, err
	}
	return c.tcpConn, nil
}

func (c *Common) GetServerUDPConn(force bool) (*net.UDPConn, error) {
	err := c.CreateServerConn(force)
	if err != nil {
		return nil, err
	}
	return c.udpConn, nil
}

func (c *Common) SetServerUDPReadChan(udpReadChan chan []byte) {
	c.udpReadChan = udpReadChan
}

func (c *Common) SetServerUDPWriteChan(udpWriteChan chan []byte) {
	c.udpWriteChan = udpWriteChan
}

func (c *Common) GetServerUDPReadChan(force bool) (chan []byte, error) {
	err := c.CreateServerConn(force)
	if err != nil {
		return nil, err
	}
	return c.udpReadChan, nil
}

func (c *Common) GetServerUDPWriteChan(force bool) (chan []byte, error) {
	err := c.CreateServerConn(force)
	if err != nil {
		return nil, err
	}
	return c.udpWriteChan, nil
}

func (c *Common) SetMetadata(metadataString string) bool {
	var err error
	c.Metadata, err = ReadMetadata(metadataString)
	if err != nil {
		log.Println("Couldn't unmarshal metadata:", err)
		return false
	}
	return true
}

func (c *Common) StartUDPReaderWriter(conn *net.UDPConn) {
	go func() {
		for {
			buffer := make([]byte, 2048)
			n, err := conn.Read(buffer)
			if err != nil {
				log.Println("Couldn't receive data from server:", err)
				if strings.Contains(err.Error(), "use of closed network connection") {
					c.udpCloseChan <- struct{}{}
					return
				}
				continue
			}

			data := make([]byte, n)
			copy(data, buffer)
			c.udpReadChan <- data
		}
	}()
	go func() {
		for {
			select {
			case data := <-c.udpWriteChan:
				_, err := conn.Write(data)
				if err != nil {
					log.Println("Couldn't send data to server:", err)
				}
			case <-c.udpCloseChan:
				return
			}
		}
	}()
}

func (c *Common) UpdateServerConn() bool {
	hasTCP := len(c.Service.TCP) > 0
	hasUDP := len(c.Service.UDP) > 0

	var err error
	if hasTCP || c.ReverseMetadata != nil {
		Close(c.tcpConn)

		address := c.Metadata.IP + ":" + strconv.Itoa(c.Metadata.TCPPort)
		c.tcpConn, err = net.DialTimeout(
			string(TCP),
			address,
			time.Duration(c.DialTimeout)*time.Second,
		)
		if err != nil {
			log.Println("Couldn't connect to TCP address", address, "because:", err)
			return false
		}
		log.Println("Connected to TCP at", address)
	}
	if hasUDP || c.ReverseMetadata != nil {
		Close(c.udpConn)

		address := net.UDPAddr{IP: net.ParseIP(c.Metadata.IP), Port: c.Metadata.UDPPort}
		c.udpConn, err = net.DialUDP(
			string(UDP),
			nil,
			&address,
		)
		if err != nil {
			log.Println("Couldn't connect to UDP address", address, "because:", err)
			return false
		}
		log.Println("Connected to UDP at", address)

		c.StartUDPReaderWriter(c.udpConn)
	}
	c.connected = true

	return true
}

func (c *Common) CreateServerConn(force bool) error {
	if !c.Reverse && (c.connected == false || force) {
		topic := c.SubscriptionPrefix + c.Service.Name
	RandomSubscriber:
		for {
			c.PaymentReceiver = ""
			subscribersCount, err := c.Wallet.GetSubscribersCount(topic)
			if err != nil {
				return err
			}
			if subscribersCount == 0 {
				return errors.New("there is no service providers for " + c.Service.Name)
			}
			offset := uint32(rand.Intn(int(subscribersCount)))
			subscribers, _, err := c.Wallet.GetSubscribers(topic, offset, 1, true, false)
			if err != nil {
				return err
			}

			for subscriber, metadataString := range subscribers {
				if !c.SetMetadata(metadataString) {
					continue RandomSubscriber
				}

				if len(c.Metadata.BeneficiaryAddr) > 0 {
					c.PaymentReceiver = c.Metadata.BeneficiaryAddr
				} else {
					_, publicKey, _, err := address.ParseClientAddress(subscriber)
					if err != nil {
						log.Println(err)
						continue RandomSubscriber
					}

					pubKey, err := crypto.NewPubKeyFromBytes(publicKey)
					if err != nil {
						log.Println(err)
						continue RandomSubscriber
					}

					programHash, err := program.CreateProgramHash(pubKey)
					if err != nil {
						log.Println(err)
						continue RandomSubscriber
					}

					address, err := programHash.ToAddress()
					if err != nil {
						log.Println(err)
						continue RandomSubscriber
					}

					c.PaymentReceiver = address
				}

				entryToExitPrice, exitToEntryPrice, err := ParsePrice(c.Metadata.Price)
				if err != nil {
					log.Println(err)
					continue RandomSubscriber
				}

				if entryToExitPrice > c.EntryToExitMaxPrice {
					log.Printf("Entry to exit price %s is bigger than max allowed price %s\n", entryToExitPrice.String(), c.EntryToExitMaxPrice.String())
					continue RandomSubscriber
				}
				if exitToEntryPrice > c.ExitToEntryMaxPrice {
					log.Printf("Exit to entry price %s is bigger than max allowed price %s\n", exitToEntryPrice.String(), c.ExitToEntryMaxPrice.String())
					continue RandomSubscriber
				}

				c.EntryToExitPrice = entryToExitPrice
				c.ExitToEntryPrice = exitToEntryPrice

				if c.ReverseMetadata != nil {
					c.Metadata.ServiceTCP = c.ReverseMetadata.ServiceTCP
					c.Metadata.ServiceUDP = c.ReverseMetadata.ServiceUDP
				}

				if !c.UpdateServerConn() {
					continue RandomSubscriber
				}

				break RandomSubscriber
			}
		}
	}

	return nil
}

func ReadMetadata(metadataString string) (*Metadata, error) {
	metadata := &Metadata{}
	err := json.Unmarshal([]byte(metadataString), metadata)
	return metadata, err
}

func CreateRawMetadata(
	serviceId byte,
	serviceTCP []int,
	serviceUDP []int,
	ip string,
	tcpPort int,
	udpPort int,
	price string,
	beneficiaryAddr string,
) []byte {
	metadata := Metadata{
		IP:              ip,
		TCPPort:         tcpPort,
		UDPPort:         udpPort,
		ServiceId:       serviceId,
		ServiceTCP:      serviceTCP,
		ServiceUDP:      serviceUDP,
		Price:           price,
		BeneficiaryAddr: beneficiaryAddr,
	}
	metadataRaw, err := json.Marshal(metadata)
	if err != nil {
		log.Fatalln(err)
	}
	return metadataRaw
}

func UpdateMetadata(
	serviceName string,
	serviceId byte,
	serviceTCP []int,
	serviceUDP []int,
	ip string,
	tcpPort int,
	udpPort int,
	price string,
	beneficiaryAddr string,
	subscriptionPrefix string,
	subscriptionDuration uint32,
	subscriptionFee string,
	wallet *WalletSDK,
) {
	metadataRaw := CreateRawMetadata(
		serviceId,
		serviceTCP,
		serviceUDP,
		ip,
		tcpPort,
		udpPort,
		price,
		beneficiaryAddr,
	)
	topic := subscriptionPrefix + serviceName
	go func() {
		var waitTime time.Duration
		for {
			txid, err := wallet.Subscribe(
				"",
				topic,
				subscriptionDuration,
				string(metadataRaw),
				subscriptionFee,
			)
			if err != nil {
				waitTime = time.Second
				log.Println("Couldn't subscribe to topic", topic, "because:", err)
			} else {
				if subscriptionDuration > 3 {
					waitTime = time.Duration(subscriptionDuration-3) * config.ConsensusDuration
				} else {
					waitTime = config.ConsensusDuration
				}
				log.Println("Subscribed to topic", topic, "successfully:", txid)
			}

			time.Sleep(waitTime)
		}
	}()
}

func copyBuffer(dest io.Writer, src io.Reader, written *uint64) error {
	buf := make([]byte, 32768)
	for {
		nr, err := src.Read(buf)
		if nr > 0 {
			nw, err := dest.Write(buf[0:nr])
			if nw > 0 {
				if written != nil {
					atomic.AddUint64(written, uint64(nw))
				}
			}
			if err != nil {
				return err
			}
			if nr != nw {
				return io.ErrShortWrite
			}
		}
		if err != nil {
			if err != io.EOF {
				return err
			}
			return nil
		}
	}
}

func Pipe(dest io.WriteCloser, src io.ReadCloser, written *uint64) {
	defer dest.Close()
	defer src.Close()
	copyBuffer(dest, src, written)
}

func Close(conn io.Closer) {
	if conn == nil || reflect.ValueOf(conn).IsNil() {
		return
	}
	err := conn.Close()
	if err != nil {
		log.Println("Error while closing:", err)
	}
}

func ReadJson(fileName string, value interface{}) error {
	file, err := ioutil.ReadFile(fileName)
	if err != nil {
		return fmt.Errorf("read file error: %v", err)
	}

	err = json.Unmarshal(file, value)
	if err != nil {
		return fmt.Errorf("parse json error: %v", err)
	}

	return nil
}

func GetConnIdString(data []byte) string {
	return strconv.Itoa(int(*(*uint16)(unsafe.Pointer(&data[0]))))
}

func LoadPassword(path string) (string, error) {
	content, err := ioutil.ReadFile(path)
	if err != nil {
		return "", err
	}
	// Remove the UTF-8 Byte Order Mark
	content = bytes.TrimPrefix(content, []byte("\xef\xbb\xbf"))
	return strings.Trim(string(content), "\r\n"), nil
}

func LoadOrCreateAccount(walletFile, passwordFile string) (*vault.Account, error) {
	var wallet *vault.WalletImpl
	pswd, _ := LoadPassword(passwordFile)
	if _, err := os.Stat(walletFile); os.IsNotExist(err) {
		if len(pswd) == 0 {
			pswd = base64.StdEncoding.EncodeToString(util.RandomBytes(24))
			log.Println("Creating wallet.pswd")
			err = ioutil.WriteFile(passwordFile, []byte(pswd), 0644)
			if err != nil {
				return nil, fmt.Errorf("save password to file error: %v", err)
			}
		}
		log.Println("Creating wallet.json")
		wallet, err = vault.NewWallet(walletFile, []byte(pswd), true)
		if err != nil {
			return nil, fmt.Errorf("create wallet error: %v", err)
		}
	} else {
		if len(pswd) == 0 {
			return nil, fmt.Errorf("cannot load password from file %s", passwordFile)
		}
		wallet, err = vault.OpenWallet(walletFile, []byte(pswd))
		if err != nil {
			return nil, fmt.Errorf("open wallet error: %v", err)
		}
	}
	return wallet.GetDefaultAccount()
}
