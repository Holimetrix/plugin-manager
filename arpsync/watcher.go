package arpsync

import (
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/rancher/go-rancher-metadata/metadata"
	"github.com/vishvananda/netlink"
)

var (
	// DefaultSyncInterval specifies the default value for arpsync interval in seconds
	DefaultSyncInterval = 120
)

// ARPTableWatcher checks the ARP table periodically for invalid entries
// and programs the appropriate ones if necessary based on info available
// from rancher-metadata
type ARPTableWatcher struct {
	syncInterval time.Duration
	mc           metadata.Client
}

// Watch starts the go routine to periodically check the ARP table
// for any discrepancies
func Watch(syncIntervalStr string, mc metadata.Client) error {
	logrus.Debugf("arpsync: syncIntervalStr: %v", syncIntervalStr)

	syncInterval := DefaultSyncInterval
	if i, err := strconv.Atoi(syncIntervalStr); err == nil {
		syncInterval = i

	}

	atw := &ARPTableWatcher{
		syncInterval: time.Duration(syncInterval) * time.Second,
		mc:           mc,
	}

	go atw.syncLoop()

	return nil
}

// getBridgeInfo returns the name of the bridge used by the CNI plugin
// and also the subnet used.
func getBridgeInfo(network metadata.Network) (string, string, error) {

	bridge := ""
	bridgeSubnet := ""
	conf, _ := network.Metadata["cniConfig"].(map[string]interface{})
	for _, file := range conf {
		props, _ := file.(map[string]interface{})
		cniType, _ := props["type"].(string)
		checkBridge, _ := props["bridge"].(string)
		checkBridgeSubnet, _ := props["bridgeSubnet"].(string)

		if cniType == "rancher-bridge" {
			if checkBridge != "" {
				bridge = checkBridge
			} else {
				return "", "", fmt.Errorf("error: bridge is empty in CNI config")
			}
			if checkBridgeSubnet != "" {
				bridgeSubnet = checkBridgeSubnet
			} else {
				return "", "", fmt.Errorf("error: bridgeSubnet is empty in CNI config")
			}
			return bridge, bridgeSubnet, nil
		}
	}

	return "", "", fmt.Errorf("arpsync: couldn't find bridge info")
}

func buildContainersMap(containers []metadata.Container, network metadata.Network) (map[string]*metadata.Container, error) {
	containersMap := make(map[string]*metadata.Container)

	for index, aContainer := range containers {
		if !(aContainer.PrimaryIp != "" && aContainer.NetworkUUID == network.UUID) {
			continue
		}
		containersMap[aContainer.PrimaryIp] = &containers[index]
	}

	return containersMap, nil
}

func (atw *ARPTableWatcher) syncLoop() {

	logrus.Infof("arpsync: starting monitoring every %v seconds", atw.syncInterval)
	for {
		time.Sleep(atw.syncInterval)
		logrus.Debugf("arpsync: time to sync ARP table")
		err := atw.doSync()
		if err != nil {
			logrus.Errorf("arpsync: while syncing, got error: %v", err)
		}
	}
}

func (atw *ARPTableWatcher) doSync() error {
	logrus.Debugf("arpsync: checking the ARP table")
	networks, err := atw.mc.GetNetworks()
	if err != nil {
		logrus.Errorf("arpsync: error fetching networks from metadata")
		return err
	}

	host, err := atw.mc.GetSelfHost()
	if err != nil {
		logrus.Errorf("arpsync: error fetching self host from metadata")
		return err
	}

	services, err := atw.mc.GetServices()
	if err != nil {
		logrus.Errorf("arpsync: error fetching services from metadata")
		return err
	}

	var networkDriverMacAddress string
	localNetworks := map[string]bool{}
	for _, service := range services {
		// Trick to select the primary service of the network plugin
		// stack
		// TODO: Need to check if it's needed for Calico?
		if !(service.Kind == "networkDriverService" &&
			service.Name == service.PrimaryServiceName) {
			continue
		}

		logrus.Debugf("arpsync: service: %#v", service)
		for _, aContainer := range service.Containers {
			if aContainer.HostUUID == host.UUID {
				networkDriverMacAddress = aContainer.PrimaryMacAddress
				localNetworks[aContainer.NetworkUUID] = true
			}
		}
	}
	if len(localNetworks) == 0 {
		return fmt.Errorf("couldn't find any local networks")
	}
	logrus.Debugf("arpsync: localNetworks: %v", localNetworks)
	logrus.Debugf("arpsync: networkDriverMacAddress=%v", networkDriverMacAddress)

	var localNetwork metadata.Network
	for _, aNetwork := range networks {
		if _, ok := localNetworks[aNetwork.UUID]; ok {
			localNetwork = aNetwork
			break
		}
	}
	logrus.Debugf("arpsync: localNetwork: %+v", localNetwork)

	// Get the network config
	bridge, bridgeSubnetStr, err := getBridgeInfo(localNetwork)
	if err != nil {
		return err
	}
	logrus.Debugf("arpsync: bridge=%v, bridgeSubnet=%v", bridge, bridgeSubnetStr)

	bridgeLink, err := netlink.LinkByName(bridge)
	if err != nil {
		logrus.Errorf("arpsync: error fetching LinkByName for bridge: %v", bridge)
		return err
	}
	logrus.Debugf("arpsync: bridgeLink=%#v", bridgeLink)

	_, bridgeSubnet, err := net.ParseCIDR(bridgeSubnetStr)
	if err != nil {
		logrus.Errorf("arpsync: error parsing bridgeSubnet: %v", bridgeSubnetStr)
		return err
	}

	// Read the ARP table
	entries, err := netlink.NeighList(0, netlink.FAMILY_V4)
	if err != nil {
		logrus.Errorf("arpsync: error fetching entries from ARP table")
		return err
	}
	logrus.Debugf("arpsync: entries=%+v", entries)

	containers, err := atw.mc.GetContainers()
	if err != nil {
		logrus.Errorf("arpsync: error fetching containers from metadata")
		return err
	}
	containersMap, err := buildContainersMap(containers, localNetwork)
	//logrus.Debugf("arpsync: containersMap: %v", containersMap)

	// We only care about Rancher Managed IP addresses and
	// the IP Address of Rancher Metadata
	bridgeLinkIndex := bridgeLink.Attrs().Index
	for _, aEntry := range entries {
		if !(aEntry.LinkIndex == bridgeLinkIndex && bridgeSubnet.Contains(aEntry.IP)) {
			continue
		}
		//logrus.Debugf("arpsync: aEntry: %+v", aEntry)
		if container, found := containersMap[aEntry.IP.String()]; found {
			if container.HostUUID == host.UUID {
				if container.PrimaryMacAddress != aEntry.HardwareAddr.String() {
					logrus.Infof("arpsync: wrong ARP entry found=%+v(expected: %v) for local container, fixing it", aEntry, container.PrimaryMacAddress)

					var newHardwareAddr net.HardwareAddr
					if newHardwareAddr, err = net.ParseMAC(container.PrimaryMacAddress); err != nil {
						logrus.Errorf("arpsync: couldn't parse MAC address(%v): %v", container.PrimaryMacAddress, err)
						continue
					}
					newEntry := aEntry
					newEntry.HardwareAddr = newHardwareAddr
					newEntry.Type = netlink.NUD_REACHABLE
					if err := netlink.NeighSet(&newEntry); err != nil {
						logrus.Errorf("arpsync: error changing ARP entry: %v", err)
					}
				}
			} else {
				if aEntry.HardwareAddr.String() != networkDriverMacAddress {
					logrus.Errorf("arpsync: wrong ARP entry found=%+v(expected: %v) for remote container, fixing it", aEntry, networkDriverMacAddress)

					var newHardwareAddr net.HardwareAddr
					if newHardwareAddr, err = net.ParseMAC(networkDriverMacAddress); err != nil {
						logrus.Errorf("arpsync: couldn't parse MAC address(%v): %v", networkDriverMacAddress, err)
						continue
					}
					newEntry := aEntry
					newEntry.HardwareAddr = newHardwareAddr
					newEntry.Type = netlink.NUD_REACHABLE
					if err := netlink.NeighSet(&newEntry); err != nil {
						logrus.Errorf("arpsync: error changing ARP entry: %v", err)
					}
				}
			}
		} else {
			logrus.Debugf("arpsync: container not found for ARP entry: %+v", aEntry)
			//if err := netlink.NeighDel(&aEntry); err != nil {
			//	logrus.Errorf("arpsync: error deleting ARP entry(%+v): %v", aEntry, err)
			//	continue
			//}
		}
	}

	return nil
}
