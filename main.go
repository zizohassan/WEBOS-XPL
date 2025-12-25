package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

//
// ================= CONFIG =================
//

type Config struct {
	ClientKeys   map[string]string `json:"client_keys"`
	MacAddresses map[string]string `json:"mac_addresses"`
}

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".lg_tv_config.json")
}

func loadConfig() *Config {
	cfg := &Config{
		ClientKeys:   map[string]string{},
		MacAddresses: map[string]string{},
	}
	if data, err := os.ReadFile(configPath()); err == nil {
		_ = json.Unmarshal(data, cfg)
	}
	return cfg
}

func (c *Config) save() {
	data, _ := json.MarshalIndent(c, "", "  ")
	_ = os.WriteFile(configPath(), data, 0600)
}

//
// ================= WOL =================
//

func sendWOL(mac string) error {
	mac = strings.ReplaceAll(strings.ReplaceAll(mac, ":", ""), "-", "")
	hw, err := hex.DecodeString(mac)
	if err != nil {
		return fmt.Errorf("invalid MAC address: %v", err)
	}

	packet := make([]byte, 102)
	for i := 0; i < 6; i++ {
		packet[i] = 0xff
	}
	for i := 6; i < 102; i += 6 {
		copy(packet[i:i+6], hw)
	}

	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{
		IP:   net.IPv4bcast,
		Port: 9,
	})
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = conn.Write(packet)
	return err
}

// Check if TV is reachable
func checkTVOnline(ip string) bool {
	conn, err := net.DialTimeout("tcp", ip+":3000", 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

//
// ================= LG TV =================
//

type LGTV struct {
	IP        string
	MAC       string
	ClientKey string
	conn      *websocket.Conn
	msgID     int
	responses chan Response
}

type Message struct {
	Type    string                 `json:"type"`
	ID      string                 `json:"id,omitempty"`
	URI     string                 `json:"uri,omitempty"`
	Payload map[string]interface{} `json:"payload,omitempty"`
}

type Response struct {
	Type    string                 `json:"type"`
	ID      string                 `json:"id,omitempty"`
	Payload map[string]interface{} `json:"payload,omitempty"`
	Error   string                 `json:"error,omitempty"`
}

func (tv *LGTV) Connect() error {
	tv.responses = make(chan Response, 10)

	// Check if TV is online
	if !checkTVOnline(tv.IP) {
		if tv.MAC == "" {
			fmt.Println("📺 TV appears to be off, but no MAC address provided for WOL.")
			fmt.Println("📡 Please provide a MAC address at least once to enable remote wake.")
		} else {
			fmt.Println("📺 TV appears to be off")
			fmt.Println("📡 Sending Wake-on-LAN signal...")

			if err := sendWOL(tv.MAC); err != nil {
				return fmt.Errorf("failed to send WOL: %v", err)
			}

			fmt.Println("⏳ Waiting 30 seconds for TV to wake up...")
			time.Sleep(30 * time.Second)
		}
	}

	// Try to connect with retries
	var err error
	fmt.Println("🔌 Attempting to connect...")

	for i := 0; i < 5; i++ {
		tv.conn, _, err = websocket.DefaultDialer.Dial(
			fmt.Sprintf("ws://%s:3000/", tv.IP), nil)
		if err == nil {
			break
		}

		if i < 4 {
			fmt.Printf("⏳ Retry %d/5...\n", i+2)
			time.Sleep(3 * time.Second)
		}
	}

	if err != nil {
		return fmt.Errorf("failed to connect after retries: %v", err)
	}

	fmt.Println("✅ Connected to TV")

	go tv.listen()

	// Register with TV
	register := Message{
		Type: "register",
		ID:   "reg_0",
		Payload: map[string]interface{}{
			"pairingType": "PROMPT",
			"manifest": map[string]interface{}{
				"appVersion":      "1.0",
				"manifestVersion": 1,
				"permissions": []string{
					"LAUNCH",
					"LAUNCH_WEBAPP",
					"APP_TO_APP",
					"CONTROL_AUDIO",
					"CONTROL_DISPLAY",
					"CONTROL_INPUT_JOYSTICK",
					"CONTROL_INPUT_MEDIA_RECORDING",
					"CONTROL_INPUT_MEDIA_PLAYBACK",
					"CONTROL_INPUT_TV",
					"CONTROL_POWER",
					"READ_APP_STATUS",
					"READ_CURRENT_CHANNEL",
					"READ_INPUT_DEVICE_LIST",
					"READ_NETWORK_STATE",
					"READ_RUNNING_APPS",
					"READ_TV_CHANNEL_LIST",
					"WRITE_NOTIFICATION_TOAST",
					"READ_POWER_STATE",
					"READ_COUNTRY_INFO",
				},
			},
		},
	}

	if tv.ClientKey != "" {
		register.Payload["client-key"] = tv.ClientKey
	}

	if err := tv.conn.WriteJSON(register); err != nil {
		return fmt.Errorf("failed to register: %v", err)
	}

	// Wait for registration response
	time.Sleep(2 * time.Second)

	return nil
}

func (tv *LGTV) listen() {
	for {
		var r Response
		if err := tv.conn.ReadJSON(&r); err != nil {
			fmt.Println("\n🔴 Connection closed:", err)
			os.Exit(0)
		}

		if r.Type == "registered" {
			if key, ok := r.Payload["client-key"].(string); ok {
				tv.ClientKey = key
				cfg := loadConfig()
				cfg.ClientKeys[tv.IP] = key
				cfg.save()
				fmt.Println("🔑 Paired successfully & key saved")
			}
		} else if r.Type == "response" {
			if r.Error != "" {
				fmt.Printf("❌ Error: %s\n", r.Error)
			}
		}

		// Send response to channel for any waiting goroutines
		select {
		case tv.responses <- r:
		default:
		}
	}
}

func (tv *LGTV) send(uri string, payload map[string]interface{}) error {
	if tv.conn == nil {
		return fmt.Errorf("not connected")
	}

	tv.msgID++
	msg := Message{
		Type:    "request",
		ID:      fmt.Sprintf("req_%d", tv.msgID),
		URI:     uri,
		Payload: payload,
	}

	if err := tv.conn.WriteJSON(msg); err != nil {
		return fmt.Errorf("failed to send command: %v", err)
	}

	fmt.Println("✓ Command sent")
	return nil
}

//
// ================= COMMANDS =================
//

func (tv *LGTV) Play() error {
	return tv.send("ssap://media.controls/play", nil)
}

func (tv *LGTV) Pause() error {
	return tv.send("ssap://media.controls/pause", nil)
}

func (tv *LGTV) Stop() error {
	return tv.send("ssap://media.controls/stop", nil)
}

func (tv *LGTV) Rewind() error {
	return tv.send("ssap://media.controls/rewind", nil)
}

func (tv *LGTV) FastForward() error {
	return tv.send("ssap://media.controls/fastForward", nil)
}

func (tv *LGTV) VolumeUp() error {
	return tv.send("ssap://audio/volumeUp", nil)
}

func (tv *LGTV) VolumeDown() error {
	return tv.send("ssap://audio/volumeDown", nil)
}

func (tv *LGTV) SetVolume(v int) error {
	if v < 0 || v > 100 {
		return fmt.Errorf("volume must be between 0 and 100")
	}
	return tv.send("ssap://audio/setVolume", map[string]interface{}{"volume": v})
}

func (tv *LGTV) Mute() error {
	return tv.send("ssap://audio/setMute", map[string]interface{}{"mute": true})
}

func (tv *LGTV) Unmute() error {
	return tv.send("ssap://audio/setMute", map[string]interface{}{"mute": false})
}

func (tv *LGTV) ChannelUp() error {
	return tv.send("ssap://tv/channelUp", nil)
}

func (tv *LGTV) ChannelDown() error {
	return tv.send("ssap://tv/channelDown", nil)
}

func (tv *LGTV) PowerOff() error {
	fmt.Println("⚠️  Turning off TV...")
	return tv.send("ssap://system/turnOff", nil)
}

func (tv *LGTV) Toast(msg string) error {
	return tv.send("ssap://system.notifications/createToast", map[string]interface{}{
		"message": msg,
	})
}

func (tv *LGTV) LaunchNetflix() error {
	return tv.send("ssap://system.launcher/launch", map[string]interface{}{
		"id": "netflix",
	})
}

func (tv *LGTV) PlayVideoURL(url string) error {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("URL must start with http:// or https://")
	}

	return tv.send("ssap://system.launcher/open", map[string]interface{}{
		"target":   url,
		"mimeType": "video/mp4",
	})
}

func (tv *LGTV) LaunchYouTube(videoID string) error {
	params := map[string]interface{}{}
	if videoID != "" {
		params["contentId"] = videoID
	}

	return tv.send("ssap://system.launcher/launch", map[string]interface{}{
		"id":     "youtube.leanback.v4",
		"params": params,
	})
}

func (tv *LGTV) OpenURL(url string) error {
	return tv.send("ssap://system.launcher/open", map[string]interface{}{
		"target": url,
	})
}

func printBanner() {
	banner := `
 ██╗    ██╗███████╗██████╗  ██████╗ ███████╗      ██╗  ██╗██████╗ ██╗     
 ██║    ██║██╔════╝██╔══██╗██╔═══██╗██╔════╝      ╚██╗██╔╝██╔══██╗██║     
 ██║ █╗ ██║█████╗  ██████╔╝██║   ██║███████╗  █████╗╚███╔╝ ██████╔╝██║     
 ██║███╗██║██╔══╝  ██╔══██╗██║   ██║╚════██║  ╚════╝██╔██╗ ██╔═══╝ ██║     
 ╚███╔███╔╝███████╗██████╔╝╚██████╔╝███████║       ██╔╝ ██╗██║     ███████╗
  ╚══╝╚══╝ ╚══════╝╚═════╝  ╚═════╝ ╚══════╝       ╚═╝  ╚═╝╚═╝     ╚══════╝
                                                                           
                     Developed by: Abdelaziz | © 2025 All Rights Reserved
    `
	fmt.Println(banner)
}

func main() {
	printBanner()
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run main.go <ip> [mac]")
		fmt.Println("Example: go run main.go 192.168.1.100")
		fmt.Println("Example: go run main.go 192.168.1.100 AA:BB:CC:DD:EE:FF")
		return
	}

	ip := os.Args[1]
	cfg := loadConfig()
	mac := ""

	// If MAC provided in args, use it and save it
	if len(os.Args) >= 3 {
		mac = os.Args[2]
		cfg.MacAddresses[ip] = mac
		cfg.save()
	} else {
		// Use saved MAC if available
		mac = cfg.MacAddresses[ip]
	}

	tv := &LGTV{
		IP:        ip,
		MAC:       mac,
		ClientKey: cfg.ClientKeys[ip],
	}

	if err := tv.Connect(); err != nil {
		fmt.Println("❌ Connection failed:", err)
		return
	}

	r := bufio.NewReader(os.Stdin)

	for {
		fmt.Println(`
  ╔════════════════════════════════════════════════════════════════════════╗
  ║ [ BRIDGE-NODE: WEBOS-XPL ]                   [ STATUS: EXPLOIT_ACTIVE ]║
  ╠════════════════════════════════════════════════════════════════════════╣
  ║                                                                        ║
  ║  [ EXPLOIT MODULES ]                  [ SIGNAL INTERCEPTION ]          ║
  ║  ───┬───────────────────────────────   ───┬─────────────────────────── ║
  ║  01 │ INITIALIZE_PLAYBACK            06 │ INCREMENT_GAIN               ║
  ║  02 │ HALT_TRANSMISSION              07 │ DECREMENT_GAIN               ║
  ║  03 │ TERMINATE_VECTOR               08 │ CALIBRATE_SIGNAL_DB          ║
  ║  04 │ REVERSE_SEEK                   MM │ SUPPRESS_SONIC_OUTPUT        ║
  ║  05 │ FORWARD_SEEK                   UU │ RESTORE_SONIC_OUTPUT         ║
  ║                                                                        ║
  ║  [ PAYLOAD DEPLOYMENT ]               [ SPECTRUM ANALYSIS ]            ║
  ║  ───┬───────────────────────────────   ───┬─────────────────────────── ║
  ║  11 │ INJECT_NETFLIX_OBFUSCATED      09 │ FREQUENCY_SHIFT_+1           ║
  ║  12 │ YT_MODULE_BYPASS               10 │ FREQUENCY_SHIFT_-1           ║
  ║  13 │ REMOTE_URL_INTRUSION                                             ║
  ║  16 │ STREAM_HOST_EXFILTRATION        [ SYSTEM OVERRIDE ]              ║
  ║  14 │ BROADCAST_TOAST_MSG            15 │ CRITICAL_SHUTDOWN_CMD        ║
  ║                                      00 │ TERMINATE_XPL_SESSION        ║
  ║                                                                        ║
  ╚════════════════════════════════════════════════════════════════════════╝
`)
		fmt.Print("Enter command > ")
		c, _ := r.ReadString('\n')
		c = strings.TrimSpace(strings.ToLower(c))

		var err error
		switch c {
		case "1":
			err = tv.Play()
		case "2":
			err = tv.Pause()
		case "3":
			err = tv.Stop()
		case "4":
			err = tv.Rewind()
		case "5":
			err = tv.FastForward()
		case "6":
			err = tv.VolumeUp()
		case "7":
			err = tv.VolumeDown()
		case "8":
			fmt.Print("Enter volume (0-100): ")
			v, _ := r.ReadString('\n')
			i, parseErr := strconv.Atoi(strings.TrimSpace(v))
			if parseErr != nil {
				fmt.Println("❌ Invalid volume value")
				continue
			}
			err = tv.SetVolume(i)
		case "m":
			err = tv.Mute()
		case "u":
			err = tv.Unmute()
		case "9":
			err = tv.ChannelUp()
		case "10":
			err = tv.ChannelDown()
		case "11":
			err = tv.LaunchNetflix()
		case "12":
			fmt.Print("YouTube video ID (press Enter for app only): ")
			id, _ := r.ReadString('\n')
			err = tv.LaunchYouTube(strings.TrimSpace(id))
		case "13":
			fmt.Print("Enter URL: ")
			u, _ := r.ReadString('\n')
			err = tv.OpenURL(strings.TrimSpace(u))
		case "14":
			fmt.Print("Enter message: ")
			m, _ := r.ReadString('\n')
			err = tv.Toast(strings.TrimSpace(m))
		case "15":
			err = tv.PowerOff()
			if err == nil {
				fmt.Println("👋 TV shutting down. Goodbye!")
				time.Sleep(2 * time.Second)
				return
			}
		case "0":
			fmt.Println("👋 Goodbye!")
			return
		case "16":
			fmt.Print("Enter MP4 URL: ")
			u, _ := r.ReadString('\n')
			err = tv.PlayVideoURL(strings.TrimSpace(u))
		default:
			fmt.Println("❌ Invalid command")
			continue
		}

		if err != nil {
			fmt.Printf("❌ Command failed: %v\n", err)
		}

		time.Sleep(500 * time.Millisecond)
	}
}
