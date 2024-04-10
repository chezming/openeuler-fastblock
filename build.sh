#!/bin/bash

buildtype="Release"
component="osd"
usage() {
    echo "Usage: $0 [options]"
    echo "Options:"
    echo "  -t, --buildtype : can be Release or Debug"
    echo "  -c, --component : can be osd or monitor"
    exit 1
}

# to resolve the protoc-gen-gogo that not exist in rpm, and requires manual installation.
install_protoc_gen_gogo(){
  echo "install protoc-gen-gogo"
  go install github.com/gogo/protobuf/protoc-gen-gogo@v1.3.2
  cd $(go env GOPATH)/pkg/mod/github.com/gogo/protobuf@v1.3.2/protoc-gen-gogo && \
  go build -o /usr/bin/protoc-gen-gogo main.go
}

if [ "$#" -eq 0 ] || [ "$1" = "--help" ]; then
    usage
fi

# Loop through the command-line arguments
while [ "$#" -gt 0 ]; do
    case "$1" in
        -t|--buildtype)
	    shift
            buildtype="$1"
            ;;
        -c|--component)
	    shift
            component="$1"
            ;;
        *)
            echo "Unknown argument: $1"
            usage
            ;;
    esac
    shift
done

echo $buildtype
echo $component

root="$(cd $(dirname ${BASH_SOURCE[0]}) && pwd)"
if [[ -z ${CC} ]]; then export CC=/usr/bin/gcc; fi
if [[ -z ${CXX} ]]; then export CXX=/usr/bin/g++; fi

if ! command -v protoc-gen-gogo &> /dev/null; then
    install_protoc_gen_gogo
fi

(cd $root/src/msg/demo && ./gen.sh)

(cd $root/proto && ./build.sh)

if [ "$component" = "monitor" ]; then
	echo "build fastblock(fastblock-mon、fastblock-client) Golang code"
	(cd $root/monitor && make)
	exit 0
fi
if [ "$component" = "osd" ]; then
	echo "build fastblock(fastblock-osd、fastblock-vhost) C/C++ code"
	cmake -DCMAKE_BUILD_TYPE=$buildtype \
	  -B$root/build \
	  -H$root \
	  -DCMAKE_C_COMPILER=$CC \
	  -DCMAKE_CXX_COMPILER=$CXX \
	  "$@"
	
	(cd $root/build && make -j `grep -c ^processor /proc/cpuinfo`)
	exit 0
fi


