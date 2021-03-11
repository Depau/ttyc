package handlers

import (
	"bytes"
	"fmt"
	"github.com/Depau/ttyc"
	"github.com/Depau/ttyc/utils"
	"github.com/Depau/ttyc/ws"
	"github.com/containerd/console"
	"net/url"
	"os"
	"os/signal"
	"syscall"
)

// Tio-style (https://tio.github.io) console handler

const ClearSequence = "\033c"
const (
	EscapeChar     byte = 0x14 // Ctrl+T
	HelpChar       byte = '?'
	QuitChar       byte = 'q'
	ConfigChar     byte = 'c'
	DetectBaudChar byte = 'b'
	ClearChar      byte = 'l'
	CtrlTChar      byte = 't'
)

var cmdsHelp = map[byte]string{
	HelpChar:       "List available key commands",
	ConfigChar:     "Show configuration",
	DetectBaudChar: "Request baudrate detection (Wi-Se only)",
	ClearChar:      "Clear screen",
	QuitChar:       "Quit",
	CtrlTChar:      "Send ctrl-t key code",
}

type stdfdsHandler struct {
	client           *ws.Client
	console          *console.Console
	implementation   ttyc.Implementation
	sttyUrl          *url.URL
	credentials      *url.Userinfo
	server           string
	expectingCommand bool
}

func NewStdFdsHandler(client *ws.Client, implementation ttyc.Implementation, sttyURL *url.URL, credentials *url.Userinfo, server string) (tty TtyHandler, err error) {
	tty = &stdfdsHandler{
		client:           client,
		implementation:   implementation,
		credentials:      credentials,
		sttyUrl:          sttyURL,
		server:           server,
		console:          nil,
		expectingCommand: false,
	}
	return
}

func (s *stdfdsHandler) handleStdin(closeChan <-chan interface{}, inChan <-chan []byte, outChan chan<- []byte, errChan chan<- error) {
	for {
		var input []byte

		//println("SELECT handleStdin")
		select {
		case <-closeChan:
			return
		case input = <-inChan:
		}
		//println("SELECTED handleStdin")

		// Check for new EscapeChars before handling any pending ones, since we may add one back that needs to be
		// passed through
		escapePos := bytes.Index(input, []byte{EscapeChar})

		// Handle any pending commands, when EscapeChar was the last char of the previous buffer
		if s.expectingCommand {
			replacement := s.handleCommand(input[0], errChan)
			s.expectingCommand = false
			input = append(replacement, input[1:]...)

			if escapePos >= 0 {
				// Adjust the pre-existing escape char position based on the characters we added/removed to/from the
				// input buffer
				escapePos += 1 - len(replacement)
			}
		}

		// Handle new EscapeChars
		if escapePos >= 0 && escapePos == len(input)-1 {
			// Escape char is the last char, we need to handle it at the next iteration
			s.expectingCommand = true
			if len(input) == 1 {
				continue
			}
			input = input[:len(input)-1]
		} else if escapePos >= 0 {
			before := input[:escapePos]
			command := input[escapePos]
			after := input[escapePos+2:]
			replacement := s.handleCommand(command, errChan)
			input = bytes.Join([][]byte{before, after}, replacement)
		}

		// More than one escape char? I hope you're happy with your life.

		outChan <- input
	}
}

func (s *stdfdsHandler) handleCommand(command byte, errChan chan<- error) []byte {
	switch command {
	case QuitChar:
		println("")
		errChan <- fmt.Errorf("quitting")
	case ConfigChar:
		println("")
		ttyc.TtycPrintf("Configuration:\n")
		additionalServerInfo := ""
		if s.server != "" {
			additionalServerInfo = fmt.Sprintf(" (%s)", s.server)
		}
		ttyc.TtycPrintf(" Remote server: %s%s\n", s.client.WsClient.RemoteAddr().String(), additionalServerInfo)
		if s.implementation == ttyc.ImplementationWiSe {
			ttyConf, err := ttyc.GetStty(s.sttyUrl, s.credentials)
			if err == nil {
				ttyc.TtycPrintf(" Baudrate: %d\n", *ttyConf.Baudrate)
				ttyc.TtycPrintf(" Databits: %d\n", *ttyConf.Databits)
				ttyc.TtycPrintf(" Flow: soft\n")
				ttyc.TtycPrintf(" Stopbits: %d\n", *ttyConf.Stopbits)
				if ttyConf.Parity == nil {
					ttyc.TtycPrintf(" Parity: none\n")
				} else {
					ttyc.TtycPrintf(" Parity: %s\n", *ttyConf.Parity)
				}
			} else {
				ttyc.TtycPrintf("Failed to retrieve remote terminal configuration: %v\n", err)
			}
		}
	case DetectBaudChar:
		println("")
		if s.implementation == ttyc.ImplementationWiSe {
			ttyc.TtycPrintf("Requesting baud rate detection (it may take up to 10 seconds)\n")
			s.client.RequestBaudrateDetection()
		} else {
			ttyc.TtycAngryPrintf("Baud rate detection is only available for Wi-Se")
		}

	case ClearChar:
		// Clear screen using ANSI/VT100 escape code
		print(ClearSequence)
		_ = os.Stdout.Sync()
	case CtrlTChar:
		// Put back escape char into buffer
		return []byte{EscapeChar}
	case HelpChar:
		println("")
		ttyc.TtycPrintf("Key commands:\n")
		for key, val := range cmdsHelp {
			ttyc.TtycPrintf(" ctrl-t %c   %s\n", key, val)
		}
	}

	return []byte{}
}

func (s *stdfdsHandler) Run(errChan chan<- error) {
	if err := s.HandleReconnect(); err != nil {
		errChan <- err
		return
	}

	cmdHandlingChan := make(chan []byte, 1)
	go utils.CopyReaderToChan(s.client.CloseChan, os.Stdin, cmdHandlingChan, errChan)
	go s.handleStdin(s.client.CloseChan, cmdHandlingChan, s.client.Input, errChan)
	go utils.CopyChanToWriter(s.client.CloseChan, s.client.Output, os.Stdout, errChan)

	winch := make(chan os.Signal)
	defer close(winch)
	signal.Notify(winch, syscall.SIGWINCH)

	for {
		//println("SELECT stdfds Run")
		select {
		case <-s.client.CloseChan:
			//println("SELECTED stdfds Run")
			return
		case <-winch:
			//println("SELECTED stdfds Run winch")
			if winSize, err := (*s.console).Size(); err != nil {
				ttyc.Trace()
				errChan <- err
				return
			} else {
				s.client.ResizeTerminal(int(winSize.Width), int(winSize.Height))
			}
		case title := <-s.client.WinTitle:
			ttyc.TtycPrintf("Title: %s\n", title)
		case baudResult := <-s.client.DetectedBaudrate:
			approx := baudResult[0]
			measured := baudResult[1]
			if approx <= 0 {
				ttyc.TtycAngryPrintf("Baudrate detection was not successful (detection only works while input is received)\n")
				break
			}
			if measured > 0 {
				ttyc.TtycPrintf("Detected baudrate: likely %d bps (measured %d bps)\n", approx, measured)
			} else {
				ttyc.TtycPrintf("Detected baudrate: likely %d bps\n", approx)
			}
		}
	}
}

func (s *stdfdsHandler) HandleDisconnect() error {
	if s.console != nil {
		if err := (*s.console).Reset(); err != nil {
			ttyc.Trace()
			return err
		}
		s.console = nil
	}
	return nil
}

func (s *stdfdsHandler) HandleReconnect() error {
	current := console.Current()
	s.console = &current
	if err := current.SetRaw(); err != nil {
		ttyc.Trace()
		return err
	}
	winSize, err := current.Size()
	if err != nil {
		ttyc.Trace()
		return err
	}
	//println("RESIZE TERM")
	s.client.ResizeTerminal(int(winSize.Width), int(winSize.Height))
	//println("TERM RESIZED")
	return nil
}

func (s *stdfdsHandler) Close() error {
	if err := s.HandleDisconnect(); err != nil {
		return err
	}
	return nil
}
