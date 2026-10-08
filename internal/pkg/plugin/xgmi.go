package plugin

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// kfdXGMILinkType is the io_links/p2p_links "type" KFD reports for an XGMI
// (Infinity Fabric) link between two GPUs; 2 is PCIe.
const kfdXGMILinkType = 11

// xgmiPeers returns the PCI BDFs of the GPUs a KFD node reaches over XGMI,
// sorted. nodeBDF maps KFD node ids of the registered GPUs to their BDF, so
// links to CPU nodes and to GPUs the plugin does not register are left out.
func xgmiPeers(topoNodesDir string, nodeID int, nodeBDF map[int]string) []string {
	var peers []string
	seen := map[string]bool{}
	for _, dir := range []string{"io_links", "p2p_links"} {
		files, _ := filepath.Glob(filepath.Join(topoNodesDir, strconv.Itoa(nodeID), dir, "[0-9]*", "properties"))
		for _, f := range files {
			linkType, to, ok := readLink(f)
			if !ok || linkType != kfdXGMILinkType {
				continue
			}
			if bdf, known := nodeBDF[to]; known && to != nodeID && !seen[bdf] {
				seen[bdf] = true
				peers = append(peers, bdf)
			}
		}
	}
	sort.Strings(peers)
	return peers
}

// readLink reads the type and destination node of one KFD link properties file.
func readLink(path string) (linkType, nodeTo int, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = f.Close() }()
	linkType, nodeTo = -1, -1
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		switch fields[0] {
		case "type":
			linkType = v
		case "node_to":
			nodeTo = v
		}
	}
	return linkType, nodeTo, linkType >= 0 && nodeTo >= 0
}
