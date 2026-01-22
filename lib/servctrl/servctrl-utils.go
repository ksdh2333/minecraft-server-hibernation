package servctrl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"msh/lib/config"
	"msh/lib/errco"
	"msh/lib/model"
	"msh/lib/servstats"
)

// countPlayerSafe returns the number of players on the server.
//
// Players are retrived by (in order): server info, list command, internal connection count.
//
// If blacklist is enabled, only list command is used to get the complete player list.
//
// Internal connection count is reset if a more reliable method is used.
//
// no error is returned: the return integer is always meaningful
// (might be more or less reliable depending from where it retrieved).
func countPlayerSafe() int {
	var logMsh *errco.MshLog
	var playerCount int
	var method string

	errco.NewLogln(errco.TYPE_INF, errco.LVL_3, errco.ERROR_NIL, "retrieving player count...")

	// If blacklist is enabled and SuspendAllow is true, only use list command to get complete player list
	if config.ConfigRuntime.Msh.EnableBlacklist && config.ConfigRuntime.Msh.SuspendAllow {
		playerList, logMsh := getPlayerListFromListCom()
		if logMsh.Log(true) == nil {
			method = "list command (blacklist mode)"
			playerCount = len(playerList)
			
			// If all players are blacklisted, treat as no players for hibernation purposes
			if areAllPlayersBlacklisted(playerList) {
				errco.NewLogln(errco.TYPE_INF, errco.LVL_1, errco.ERROR_NIL, "all %d online players are blacklisted - server can hibernate", playerCount)
				return 0
			}
			
			errco.NewLogln(errco.TYPE_INF, errco.LVL_1, errco.ERROR_NIL, "%d online players (non-blacklisted) - method for player count: %s", playerCount, method)
			return playerCount
		} else {
			// If list command fails, fall back to connection count
			method = "connection count (list command failed)"
			playerCount = servstats.Stats.ConnCount
			errco.NewLogln(errco.TYPE_INF, errco.LVL_1, errco.ERROR_NIL, "%d online players - method for player count: %s", playerCount, method)
			return playerCount
		}
	}

	// Normal player count detection (when blacklist is not enabled)
	if playerCount, logMsh = getPlayersByServInfo(); logMsh.Log(true) == nil {
		method = "server info"
		if playerCount != servstats.Stats.ConnCount {
			errco.NewLogln(errco.TYPE_WAR, errco.LVL_1, errco.ERROR_WRONG_CONNECTION_COUNT, "connection count (%d) different from %s player count (%d)", servstats.Stats.ConnCount, method, playerCount)
		}

	} else if playerCount, logMsh = getPlayersByListCom(); logMsh.Log(true) == nil {
		method = "list command"
		if playerCount != servstats.Stats.ConnCount {
			errco.NewLogln(errco.TYPE_WAR, errco.LVL_1, errco.ERROR_WRONG_CONNECTION_COUNT, "connection count (%d) different from %s player count (%d)", servstats.Stats.ConnCount, method, playerCount)
		}

	} else {
		method = "connection count"
		playerCount = servstats.Stats.ConnCount
	}

	errco.NewLogln(errco.TYPE_INF, errco.LVL_1, errco.ERROR_NIL, "%d online players - method for player count: %s", playerCount, method)

	return playerCount
}

// getPlayersByListCom returns the number of players using "list" command
func getPlayersByListCom() (int, *errco.MshLog) {
	output, logMsh := Execute("list")
	if logMsh != nil {
		return -1, logMsh.AddTrace()
	}

	playerCount, logMsh := searchListCom(output)
	if logMsh != nil {
		return -1, logMsh.AddTrace()
	}

	return playerCount, nil
}

// searchListCom analyzes the output of the list command to extract player count
func searchListCom(s string) (int, *errco.MshLog) {
	// return if string has unexpected format
	if !strings.Contains(s, "INFO]:") {
		return -1, errco.NewLog(errco.TYPE_ERR, errco.LVL_3, errco.ERROR_SERVER_UNEXP_OUTPUT, "string does not contain \"INFO]:\"")
	}

	playerCount := regexp.MustCompile(` \d+ `).FindString(s)
	playerCount = strings.ReplaceAll(playerCount, " ", "")

	// check if playerCount has been found
	if playerCount == "" {
		return -1, errco.NewLog(errco.TYPE_ERR, errco.LVL_3, errco.ERROR_SERVER_UNEXP_OUTPUT, "player count number not found in output of list command")
	}

	players, err := strconv.Atoi(playerCount)
	if err != nil {
		return -1, errco.NewLog(errco.TYPE_ERR, errco.LVL_3, errco.ERROR_CONVERSION, err.Error())
	}

	return players, nil
}

// getPlayersByServInfo returns the number of players using server info request
func getPlayersByServInfo() (int, *errco.MshLog) {
	servInfo, logMsh := getServInfo()
	if logMsh != nil {
		return -1, logMsh.AddTrace()
	}

	return servInfo.Players.Online, nil
}

// getServInfo returns server info after emulating a server info request to the minecraft server
func getServInfo() (*model.DataInfo, *errco.MshLog) {
	var recInfoData []byte = []byte{}
	var recInfo *model.DataInfo = &model.DataInfo{}
	var buf []byte = make([]byte, 1024)

	// check if ms is warm and interactable
	logMsh := CheckMSWarm()
	if logMsh != nil {
		return nil, logMsh.AddTrace()
	}

	// open connection to minecraft server
	serverSocket, err := net.Dial("tcp", fmt.Sprintf("%s:%d", config.ServHost, config.ServPort))
	if err != nil {
		return nil, errco.NewLog(errco.TYPE_ERR, errco.LVL_3, errco.ERROR_SERVER_DIAL, err.Error())
	}
	defer serverSocket.Close()

	// building byte array to request minecraft server info
	// [16 0 244 5 9 49 50 55 46 48 46 48 46 49 99 211 1 1 0 ]
	//                                          └port┘ └info┘
	reqInfoMessage := bytes.NewBuffer([]byte{16, 0, 244, 5, 9, 49, 50, 55, 46, 48, 46, 48, 46, 49})
	reqInfoMessage.Write(big.NewInt(int64(config.MshPort)).Bytes())
	reqInfoMessage.Write([]byte{1, 1, 0})

	mes := reqInfoMessage.Bytes()
	serverSocket.Write(mes)
	errco.NewLogln(errco.TYPE_BYT, errco.LVL_4, errco.ERROR_NIL, "%smsh --> server%s: %v", errco.COLOR_PURPLE, errco.COLOR_RESET, mes)

	// read response from server
	for {
		// timeout can be low since its a connection to 127.0.0.1
		// the first time the ms info are requested it timeout is <100 mills
		// (probably the ms function that handles ms info needs time to load the first time it's called)
		serverSocket.SetReadDeadline(time.Now().Add(200 * time.Millisecond))

		dataLen, err := serverSocket.Read(buf)
		if err != nil {
			// cannot break on io.EOF since it's not sent, so break happens on timeout
			// using io.EOF would be better
			if err, ok := err.(net.Error); ok && err.Timeout() {
				break
			}

			return nil, errco.NewLog(errco.TYPE_ERR, errco.LVL_3, errco.ERROR_SERVER_REQUEST_INFO, err.Error())
		}

		errco.NewLogln(errco.TYPE_BYT, errco.LVL_4, errco.ERROR_NIL, "%sserver --> msh%s: %v", errco.COLOR_PURPLE, errco.COLOR_RESET, buf[:dataLen])

		recInfoData = append(recInfoData, buf[:dataLen]...)
	}

	// remove first 5 bytes that are used as header to get only the json data
	// [178 88 0 175 88]{"description":{ ...
	if len(recInfoData) < 5 {
		return nil, errco.NewLog(errco.TYPE_ERR, errco.LVL_3, errco.ERROR_SERVER_REQUEST_INFO, "not enough data received (%v)", recInfoData)
	}
	recInfoData = recInfoData[5:]

	// load data into struct
	err = json.Unmarshal(recInfoData, recInfo)
	if err != nil {
		return nil, errco.NewLog(errco.TYPE_ERR, errco.LVL_3, errco.ERROR_JSON_UNMARSHAL, err.Error())
	}

	// update server version and protocol in config
	if recInfo.Version.Name != config.ConfigRuntime.Server.Version || recInfo.Version.Protocol != config.ConfigRuntime.Server.Protocol {
		errco.NewLogln(errco.TYPE_INF, errco.LVL_3, errco.ERROR_NIL, "server version found! serverVersion: %s serverProtocol: %d", recInfo.Version.Name, recInfo.Version.Protocol)

		// update runtime config if version is not specified
		if config.ConfigRuntime.Server.Version == "" {
			config.ConfigRuntime.Server.Version = recInfo.Version.Name
			config.ConfigRuntime.Server.Protocol = recInfo.Version.Protocol
		}

		// update and save default config
		config.ConfigDefault.Server.Version = recInfo.Version.Name
		config.ConfigDefault.Server.Protocol = recInfo.Version.Protocol
		logMsh := config.ConfigDefault.Save()
		if logMsh != nil {
			return nil, logMsh.AddTrace()
		}
	}

	return recInfo, nil
}

// getPlayerListFromListCom returns the list of players using "list" command
func getPlayerListFromListCom() ([]string, *errco.MshLog) {
	output, logMsh := Execute("list")
	if logMsh != nil {
		return nil, logMsh.AddTrace()
	}

	playerList, logMsh := parsePlayerList(output)
	if logMsh != nil {
		return nil, logMsh.AddTrace()
	}

	return playerList, nil
}

// parsePlayerList analyzes the output of the list command to extract player list
func parsePlayerList(s string) ([]string, *errco.MshLog) {
	// return if string has unexpected format
	if !strings.Contains(s, "INFO]:") {
		return nil, errco.NewLog(errco.TYPE_ERR, errco.LVL_3, errco.ERROR_SERVER_UNEXP_OUTPUT, "string does not contain \"INFO]:\"")
	}

	// Check for different formats of list command output
	var playerNames []string
	
	// Format 1: "There are 2 players online: Player1, Player2"
	if strings.Contains(s, "players online:") {
		playerListStr := strings.Split(s, "players online:")[1]
		playerNames = strings.Split(playerListStr, ", ")
	} else if strings.Contains(s, ", ") {
		// Format 2: "Player1, Player2" (when no players or different format)
		// Extract the part after "INFO]: "
		infoPart := strings.Split(s, "INFO]: ")[1]
		playerNames = strings.Split(infoPart, ", ")
	} else {
		// Format 3: Single player or no players
		// Extract the part after "INFO]: "
		infoPart := strings.Split(s, "INFO]: ")[1]
		infoPart = strings.TrimSpace(infoPart)
		if infoPart != "" && infoPart != "There are 0 players online." {
			playerNames = []string{infoPart}
		} else {
			playerNames = []string{}
		}
	}

	// Clean up player names
	for i, name := range playerNames {
		playerNames[i] = strings.TrimSpace(name)
	}

	return playerNames, nil
}

// isPlayerInBlacklist checks if a player is in the blacklist
func isPlayerInBlacklist(playerName string) bool {
	for _, blacklistedPlayer := range config.BlacklistConfig.Blacklist {
		if strings.EqualFold(blacklistedPlayer, playerName) {
			return true
		}
	}
	return false
}

// areAllPlayersBlacklisted checks if all online players are in the blacklist
func areAllPlayersBlacklisted(playerList []string) bool {
	// If no players online, return false (server is already empty)
	if len(playerList) == 0 {
		return false
	}
	
	// Check if all players are in the blacklist
	for _, player := range playerList {
		if !isPlayerInBlacklist(player) {
			return false
		}
	}
	
	return true
}
