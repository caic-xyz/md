#!/bin/bash
# Install core system packages (runs as root).
set -euo pipefail
echo "- $0"

export DEBIAN_FRONTEND="${DEBIAN_FRONTEND:-noninteractive}"

echo "- apt-get update"
apt-get update -qq >/dev/null
echo "- apt-get upgrade"
apt-get upgrade -qq -y >/dev/null
echo "- apt-get install"
# Keep the list of packages sorted!
apt-get install -qq -y --no-install-recommends \
	bash-completion \
	bind9-dnsutils \
	binutils \
	bison \
	breeze-cursor-theme \
	brotli \
	bubblewrap \
	build-essential \
	ca-certificates \
	ccache \
	chromium \
	chromium-sandbox \
	clang \
	cmake \
	cpu-checker \
	crun \
	curl \
	dbus-x11 \
	dfu-util \
	extrepo \
	ffmpeg \
	file \
	flex \
	fonts-noto-color-emoji \
	fping \
	fuse-overlayfs \
	g++-arm-linux-gnueabihf \
	git \
	gperf \
	gpg \
	gradle \
	imagemagick \
	inotify-tools \
	iproute2 \
	iputils-ping \
	jq \
	kmod \
	less \
	libc6-dev \
	libcairo2-dev \
	libcurl4-openssl-dev \
	libffi-dev \
	libfontconfig1-dev \
	libfreetype-dev \
	libfribidi-dev \
	libgl1 \
	libharfbuzz-dev \
	libjpeg-dev \
	libopenblas-dev \
	libopus-dev \
	libpng-dev \
	librsvg2-bin \
	libssl-dev \
	libtiff-dev \
	libusb-1.0-0 \
	libuv1-dev \
	libvirt-clients \
	libvirt-daemon \
	libvirt-daemon-system \
	libwebp-dev \
	libxml2-dev \
	lldb \
	locales \
	lsof \
	meson \
	net-tools \
	ninja-build \
	nmap \
	openjdk-21-jdk-headless \
	openssh-server \
	passt \
	pkg-config \
	podman \
	python-is-python3 \
	python3 \
	qemu-kvm \
	qemu-system-arm \
	qemu-system-x86 \
	qemu-utils \
	r-base-dev \
	ripgrep \
	rsync \
	shared-mime-info \
	shellcheck \
	slirp4netns \
	sqlite3 \
	strace \
	sudo \
	tcpdump \
	tcptraceroute \
	tigervnc-standalone-server \
	tigervnc-tools \
	tigervnc-viewer \
	tokei \
	uidmap \
	unzip \
	w3m \
	wget \
	traceroute \
	whois \
	xfce4 \
	xfce4-terminal \
	xfwm4-theme-breeze \
	xvfb \
	xxd \
	zstd >/dev/null

# Register chromium as www-browser (the Debian virtual package for a web browser).
# Chromium Provides: www-browser but its postinst only sets up x-www-browser and gnome-www-browser.
update-alternatives --install /usr/bin/www-browser www-browser /usr/bin/chromium 40

# Configure R to use OpenBLAS via the alternatives system
ARCH=$(dpkg-architecture -qDEB_HOST_MULTIARCH)
update-alternatives --set "libblas.so.3-${ARCH}" "/usr/lib/${ARCH}/openblas-pthread/libblas.so.3"

# Remove PEP 668 marker — pip install --user is safe and this is a container.
rm -f /usr/lib/python3.*/EXTERNALLY-MANAGED

sed -i 's/^# - /- /g' /etc/extrepo/config.yaml
if ! grep -q '^en_US.UTF-8 UTF-8' /etc/locale.gen; then
	sed -i 's/^# *en_US.UTF-8 UTF-8/en_US.UTF-8 UTF-8/' /etc/locale.gen || echo 'en_US.UTF-8 UTF-8' >>/etc/locale.gen
fi
locale-gen en_US.UTF-8
update-locale LANG=en_US.UTF-8 LC_CTYPE=en_US.UTF-8
