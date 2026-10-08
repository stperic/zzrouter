package clusternode

// ServingDoneForTest returns a channel that closes when the serve
// goroutine has returned (HTTP listener fully torn down). Nil on a node
// that never opened a listener (Disabled/Unclaimed).
func (n *Node) ServingDoneForTest() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.servingDone
}
