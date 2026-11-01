// Package clipsync keeps the system clipboards of two machines in sync over a
// TCP connection.
//
// One peer listens for connections and the other connects to it. Both peers
// poll their local clipboard and send changes to the other side; changes
// received from the peer are written to the local clipboard. A shared token can
// be used to authenticate peers. Clipboard data is sent as plaintext JSON, so
// the connection should run over a trusted network or an encrypted tunnel.
package clipsync
