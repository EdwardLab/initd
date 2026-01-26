package ipc

import (
	"encoding/json"
	"net"
)

type Client struct {
	SocketPath string
}

func (c *Client) Do(req Request) (Response, error) {
	conn, err := net.Dial("unix", c.SocketPath)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()

	encoder := json.NewEncoder(conn)
	decoder := json.NewDecoder(conn)

	if err := encoder.Encode(req); err != nil {
		return Response{}, err
	}

	var resp Response
	if err := decoder.Decode(&resp); err != nil {
		return Response{}, err
	}
	return resp, nil
}
