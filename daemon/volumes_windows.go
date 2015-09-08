// +build windows

package daemon

import (
	"github.com/docker/docker/daemon/execdriver"
)

// copyOwnership copies the permissions and group of a source file to the
// destination file. This is a no-op on Windows.
func copyOwnership(source, destination string) error {
	return nil
}

// setupMounts configures the mount points for a container.
// setupMounts on Linux iterates through each of the mount points for a
// container and calls Setup() on each. It also looks to see if is a network
// mount such as /etc/resolv.conf, and if it is not, appends it to the array
// of mounts. As Windows does not support mount points, this is a no-op.
func (container *Container) setupMounts() ([]execdriver.Mount, error) {
	var mounts []execdriver.Mount
	for _, m := range container.MountPoints {
		path, err := m.Setup()
		if err != nil {
			return nil, err
		}
        mounts = append(mounts, execdriver.Mount{
            Source:      path,
            Destination: m.Destination,
            Writable:    m.RW,
        })
	}

	return mounts, nil
}

// verifyVolumesInfo ports volumes configured for the containers pre docker 1.7.
// As the Windows daemon was not supported before 1.7, this is a no-op
func (daemon *Daemon) verifyVolumesInfo(container *Container) error {
	return nil
}
