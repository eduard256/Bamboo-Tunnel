package protocol

// Fake video call handshake messages.
// Sent after tunnel connection established to mimic
// a real video conferencing "join room" sequence.

// ServerHandshakeMessages returns fake JSON messages that server
// sends to client after successful auth, mimicking video call setup.
func ServerHandshakeMessages() [][]byte {
	return [][]byte{
		[]byte(`{"type":"room.joined","room_id":"` + randHex(16) + `","participant_id":"` + randHex(8) + `","server_time":` + timestamp() + `}`),
		[]byte(`{"type":"media.config","audio":{"codec":"opus","sample_rate":48000,"channels":2},"video":{"codec":"vp9","width":1280,"height":720,"fps":30}}`),
		[]byte(`{"type":"room.ready","ice_servers":[],"turn_enabled":false}`),
	}
}

// ClientHandshakeMessages returns fake JSON messages that client
// sends to server when joining, mimicking video call setup.
func ClientHandshakeMessages() [][]byte {
	return [][]byte{
		[]byte(`{"type":"room.join","sdk_version":"2.8.1","platform":"web","browser":"chrome"}`),
		[]byte(`{"type":"media.offer","audio":true,"video":true,"screen":false}`),
	}
}
