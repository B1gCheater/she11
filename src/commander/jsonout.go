package commander

import (
	"encoding/json"
	"fmt"

	"she11/src/shared/proto"
)

func printJSONCheck(rows []checkRow, reachable int) {
	type outRow struct {
		Name    string `json:"name"`
		Addr    string `json:"addr"`
		Status  string `json:"status"`
		Clients int    `json:"clients"`
		Uptime  int64  `json:"uptime_sec"`
	}
	var out []outRow
	for _, r := range rows {
		out = append(out, outRow{Name: r.name, Addr: r.addr, Status: r.status,
			Clients: r.clients, Uptime: r.uptime})
	}
	b, _ := json.Marshal(map[string]interface{}{
		"servers": out, "reachable": reachable, "total": len(rows)})
	fmt.Println(string(b))
}

type checkRow struct {
	name, addr, status string
	clients            int
	uptime             int64
}

func printJSONClients(clients []proto.ClientInfo, failures []serverFailure, err error) {
	result := map[string]interface{}{"clients": clients, "total": len(clients), "server_errors": failures}
	if err != nil {
		result["error"] = err.Error()
	}
	b, _ := json.Marshal(result)
	fmt.Println(string(b))
}
