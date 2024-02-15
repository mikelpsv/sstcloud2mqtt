package main

import (
	"encoding/json"
	"fmt"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/joho/godotenv"
	sst "github.com/mikelpsv/sst_cloud_sdk"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Cfg struct {
	BaseTopic        string
	MqttHost         string
	MqttPort         string
	MqttClientId     string
	SstCloudLogin    string
	SstCloudPass     string
	SstCloudMail     string
	DeviceUpdateTime int
}

type Message struct {
	TemperatureFloor float32 `json:"temperature_floor"`
	TemperatureAir   float32 `json:"temperature_air"`
	TemperatureSet   float32 `json:"temperature_set"`
	RelayStatus      bool    `json:"relay_status"`
	SignalLevel      int     `json:"signal_level"`
	Status           bool    `json:"status"`
}

var (
	houses      []sst.House
	cloudData   map[int64]int64
	chanMsgMqtt chan mqtt.Message
	cfg         Cfg
)

func main() {

	cloudData = make(map[int64]int64)

	c := make(chan os.Signal, 1)
	chanMsgMqtt = make(chan mqtt.Message)

	signal.Notify(c, syscall.SIGTERM, syscall.SIGINT, syscall.SIGKILL)

	err := godotenv.Load()
	if err != nil {
		log.Printf("error reading env, %v", err)
		os.Exit(1)
	}
	cfg.BaseTopic = os.Getenv("BASE_TOPIC")
	cfg.MqttHost = os.Getenv("MQTT_HOST")
	cfg.MqttPort = os.Getenv("MQTT_PORT")
	cfg.MqttClientId = os.Getenv("MQTT_CLIENT_ID")
	cfg.SstCloudLogin = os.Getenv("SSTCLOUD_LOGIN")
	cfg.SstCloudPass = os.Getenv("SSTCLOUD_PASSWORD")
	cfg.SstCloudMail = os.Getenv("SSTCLOUD_MAIL")
	updateTime := os.Getenv("DEVICE_UPDATE_TIME")
	cfg.DeviceUpdateTime, err = strconv.Atoi(updateTime)
	if err != nil {
		log.Printf("error converting update time, %v", updateTime)
		os.Exit(1)
	}

	ticker := time.NewTicker(time.Duration(cfg.DeviceUpdateTime) * time.Second)

	// Подключаемся сначала к облаку, т.к. надо знать какие устройства есть, и на какие топики подписываться
	s := new(sst.Session)
	// Авторизация
	_, err = s.Login(sst.LoginRequest{Username: cfg.SstCloudLogin, Password: cfg.SstCloudPass, Email: cfg.SstCloudMail})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer s.Logout()

	err = ReadCloudData(s, cloudData)
	if err != nil {
		fmt.Println(err)
		return
	}

	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("tcp://%s:%s", cfg.MqttHost, cfg.MqttPort))
	opts.SetClientID(cfg.MqttClientId)
	opts.OnConnect = OnConnectMqtt
	opts.OnConnectionLost = OnLostConnectMqtt
	client := mqtt.NewClient(opts)

	token := client.Connect()
	if token.Wait() && token.Error() != nil {
		fmt.Println(token.Error())
		os.Exit(1)
	}
	defer client.Disconnect(3000)

	for {
		select {
		case <-ticker.C:
			for _, h := range houses {
				for _, v := range s.Cookies {
					if v.Expires.UTC().After(time.Now().UTC()) {
						continue
					}
					s.Login(sst.LoginRequest{Username: cfg.SstCloudLogin, Password: cfg.SstCloudPass, Email: cfg.SstCloudMail})
				}

				devices, err := s.GetDevices(h.Id)
				if err != nil {
					fmt.Println(err)
					continue
				}
				for _, d := range devices {
					cf, _ := d.ReadConfigThermostat()
					m := Message{
						TemperatureFloor: float32(cf.CurrentTemperature.TemperatureFloor),
						TemperatureAir:   float32(cf.CurrentTemperature.TemperatureAir),
						TemperatureSet:   float32(cf.Settings.TemperatureManual),
						RelayStatus:      cf.RelayStatus == "on",
						SignalLevel:      cf.SignalLevel,
					}
					mData, err := json.Marshal(m)
					if err != nil {
						continue
					}
					topic := fmt.Sprintf("%s/%d/%d/status", cfg.BaseTopic, d.HouseId, d.Id)
					client.Publish(topic, 1, false, mData)
					fmt.Printf("Public %s, value %s \n", topic, mData)
				}
			}
		case m := <-chanMsgMqtt:
			str := strings.Split(m.Topic(), "/")
			if len(str) == 5 {
				house := str[1]
				device := str[2]
				function := str[3]
				param := str[4]

				houseId, _ := strconv.Atoi(house)
				deviceId, _ := strconv.Atoi(device)
				if function == "set" {
					if param == "temperature_set" {
						tempVal, err := strconv.Atoi(string(m.Payload()))
						if err != nil {
							fmt.Printf("Invalid temperature value, house %s, device %s, data: %v", house, device, string(m.Payload()))
						}
						t := sst.TemperatureSet{TemperatureManual: tempVal}
						s.SetTemperature(houseId, deviceId, &t)
					}
				}

				fmt.Printf("%s %s %s %s - %v", house, device, function, param, string(m.Payload()))

			}
		case <-c:
			os.Exit(0)
		}
	}
}

func ReadCloudData(s *sst.Session, cloudData map[int64]int64) error {
	var err error
	if houses == nil {
		// Чтение домовладений текущего аккаунта
		houses, err = s.GetHouses()
		if err != nil {
			return err
		}
	}

	for _, h := range houses {
		devs, err := s.GetDevices(h.Id)
		if err != nil {
			return err
		}
		for _, d := range devs {
			cloudData[d.Id] = h.Id
		}
	}
	return nil
}

func OnConnectMqtt(client mqtt.Client) {
	fmt.Println("Connected")

	for d, h := range cloudData {
		topic := fmt.Sprintf("%s/%d/%d/set/+", cfg.BaseTopic, h, d)
		if token := client.Subscribe(topic, 0, OnReceiveMqttMsg); token.Wait() && token.Error() != nil {
			fmt.Println(token.Error())
			os.Exit(1)
		}
		fmt.Printf("Client connected, subscribing to: %s\n", topic)
	}
}

func OnLostConnectMqtt(client mqtt.Client, err error) {
	fmt.Printf("Connect lost: %v", err)
}

func OnReceiveMqttMsg(client mqtt.Client, msg mqtt.Message) {
	fmt.Printf("Received message: %s from topic: %s\n", msg.Payload(), msg.Topic())
	chanMsgMqtt <- msg
}
