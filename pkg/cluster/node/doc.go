// Package clusternode owns node identity, cluster trust and enrollment.
// Coordinators initiate mTLS requests; nodes publish operations through held polls.
// Unclaimed nodes expose only an explicitly armed, expiring enrollment listener.
//
// Enrollment acquires pairingMu before mu for listener admission and trust commit.
// Stop claims lifecycle state under mu, releases it, then handles pairing state.
// No path may acquire pairingMu while holding mu. Role callbacks run unlocked.
// The deny list and enrollment rate limiter own independent internal locks.
//
// Node owns listener, enrollment and renewal goroutines. Signaler owns pending
// operations; the coordinator poll owner lives with the server's coordinator role.
package clusternode
