package utils

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/containernetworking/cni/pkg/ns"
	"github.com/docker/engine-api/client"
	"github.com/docker/engine-api/types"
	//"github.com/pkg/errors"
	"github.com/rancher/go-rancher-metadata/metadata"
	"github.com/rancher/plugin-manager/network"
	"github.com/vishvananda/netlink"
)

// GetHostViewVethMap returns a map of veths as seen from host
func GetHostViewVethMap(vethPrefix string, mc metadata.Client) (map[string]*netlink.Link, error) {
	// get docker bridge
	veths := make(map[string]*netlink.Link)

	alllinks, err := netlink.LinkList()
	if err != nil {
		logrus.Errorf("vethsync/utils: error getting links: %v", err)
		return nil, err
	}

	localNetworks, _, err := network.LocalNetworks(mc)
	if err != nil {
		logrus.Errorf("vethsync/utils: error fetching local networks: %v", err)
		return nil, err
	}
	logrus.Debugf("vethsync/utils: localNetworks: %v", localNetworks)

	localBridges := make(map[string]bool)
	for _, n := range localNetworks {
		cniConf, ok := n.Metadata["cniConfig"].(map[string]interface{})
		if !ok {
			continue
		}

		b, err := getBridgeInfoFromCNIConfig(cniConf)
		if err != nil {
			continue
		}
		localBridges[b] = true
	}

	localBridgesLinksMap := make(map[int]*netlink.Link)
	for index, l := range alllinks {
		if _, found := localBridges[l.Attrs().Name]; found {
			localBridgesLinksMap[l.Attrs().Index] = &alllinks[index]
			logrus.Debugf("vethsync/utils: found bridge link: %v", l)
		}
	}

	if len(localBridgesLinksMap) == 0 {
		err = fmt.Errorf("couldn't find any local bridge link")
		logrus.Errorf("vethsync/utils: %v", err)
		return nil, err
	}
	logrus.Debugf("vethsync/utils: localBridgesLinksMap: %v", localBridgesLinksMap)

	for index, l := range alllinks {
		if !strings.HasPrefix(l.Attrs().Name, vethPrefix) {
			continue
		}
		if _, found := localBridgesLinksMap[l.Attrs().MasterIndex]; !found {
			continue
		}
		veths[strconv.Itoa(l.Attrs().Index)] = &alllinks[index]
	}

	return veths, nil
}

func getBridgeInfoFromCNIConfig(cniConf map[string]interface{}) (string, error) {
	var lastErr error
	var bridge string
	for _, config := range cniConf {
		props, ok := config.(map[string]interface{})
		if !ok {
			err := fmt.Errorf("error getting props from cni config")
			logrus.Errorf("vethsync/utils: %v", err)
			lastErr = err
			continue
		}
		bridge, ok = props["bridge"].(string)
		if !ok {
			err := fmt.Errorf("error getting bridge from cni config")
			logrus.Errorf("vethsync/utils: %v", err)
			lastErr = err
			continue
		}
	}

	logrus.Debugf("vethsync/utils: bridge: %v", bridge)
	return bridge, lastErr
}

// GetContainersViewVethMapByEnteringNS returns a map of veth indices as seen
// by containers by entering their network namespace.
func GetContainersViewVethMapByEnteringNS(dc *client.Client) (map[string]bool, error) {
	containers, err := dc.ContainerList(context.Background(), types.ContainerListOptions{})
	if err != nil {
		logrus.Errorf("vethsync/utils: error fetching containers from docker client: %v", err)
		return nil, err
	}
	containerVethIndices := map[string]bool{}
	for _, aContainer := range containers {
		if aContainer.HostConfig.NetworkMode == "host" {
			continue
		}

		var vethIndex string
		err := network.EnterNS(dc, aContainer.ID, func(n ns.NetNS) error {
			link, err := netlink.LinkByName("eth0")
			if err != nil {
				return err
			}
			vethIndex = strconv.Itoa(link.Attrs().ParentIndex)
			return nil
		})
		if err != nil {
			logrus.Errorf("vethsync/utils: error figuring out the vethIndex for container %v: %v", aContainer.ID, err)
			continue
		}
		logrus.Debugf("vethsync/utils: for container %v got vethIndex: %v", aContainer.ID, vethIndex)
		containerVethIndices[vethIndex] = true
	}

	return containerVethIndices, nil
}

// GetContainersViewVethMapUsingID returns a map of peer veth indices as seen by
// containers by using docker IDs.
func GetContainersViewVethMapUsingID(dc *client.Client) (map[string]bool, error) {
	containers, err := dc.ContainerList(context.Background(), types.ContainerListOptions{})
	if err != nil {
		logrus.Errorf("vethsync/utils: error fetching containers from docker client: %v", err)
		return nil, err
	}
	containerVethIndices := map[string]bool{}
	for _, aContainer := range containers {
		if aContainer.HostConfig.NetworkMode == "host" {
			continue
		}
		index := fmt.Sprintf("vethr%v", aContainer.ID[:10])
		containerVethIndices[index] = true
	}

	return containerVethIndices, nil
}

// GetDanglingVeths compares the host view of the veths and the containers view of
// veths to figure out if there are any dangling veths present
func GetDanglingVeths(
	hostVethMap map[string]*netlink.Link, containerVethMap map[string]bool) (map[string]*netlink.Link, error) {
	logrus.Debugf("vethsync/utils: checking for dangling veths")

	dangling := make(map[string]*netlink.Link)
	for k, v := range hostVethMap {
		_, found := containerVethMap[k]
		if !found {
			logrus.Debugf("vethsync/utils: dangling veth found: %v", *v)
			dangling[k] = v
		}
	}

	return dangling, nil
}

// CleanUpDanglingVeths deletes the given dangling veths from the host
func CleanUpDanglingVeths(dangling map[string]*netlink.Link) error {
	logrus.Debugf("vethsync/utils: cleaning up dangling veths")
	for _, v := range dangling {
		if err := netlink.LinkDel(*v); err != nil {
			logrus.Errorf("vethsync/utils: error deleting dangling veth: %v", *v)
			continue
		}
	}
	return nil
}
