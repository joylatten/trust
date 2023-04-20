#!/bin/bash
# doinitrd
# This tool does the following:
# 1. copies the initrd from the kernel.efi
# 2. unpacks the initrd
# 3. swaps out the manifest* cert(s)
# 4. repacks the initrd and place it into the PE of the kernel.efi
# 5. resigns the kernel.efi with appropriate key
set -x

BOOTKIT_VERSION="0.0.5.230327-squashfs"
BOOTKIT_URL="docker://zothub.io/machine/bootkit/bootkit:${BOOTKIT_VERSION}"
WORKSPACE=""

cleanup() {
	sudo rm -rf $WORKSPACE
}

do_initrd() {
	local uki=$1
	local keysetPath=$2
	local product=$3
	local numBlocks="" sum=""

	cwd = $(pwd)
	# copy out the initrd from the PE of kernel.efi and cd to temp workspace
	objcopy -O binary -j .initrd "$uki" "$WORKSPACE/initrd.gz" && cd "$WORKSPACE" || {
		stderr "do_initrd: objcopy failed"
		return 1
	}

	# unpack initrd
	gunzip initrd.gz && mkdir initrd-tmp && cd initrd-tmp || {
		stderr "do_kernel: failed to prep initrd for extracting"
		return 1
	}

	# first cpio
	sudo cpio -idt --no-absolute-filenames < ../initrd.out 2> cpio.out || {
		stderr "first cpio failed"
		return 1
	}
    # get the number of blocks
    numBlocks=$(grep [0-9][[:space:]]block cpio.out | cut -d ' ' -f 1)
    [ -n "$numBlocks" ] || { stderr "failed to get num of blocks from cpio output"; return 1; }

	# start a loop
	local sum=$numBlocks
	while [ -n "$numBlocks" ]; do
		dd if=../initrd.out skip=$sum | sudo cpio -id --no-absolute-filenames 2> cpio.out
		numBlocks=$(grep -E '[0-9]\s+block' cpio.out | cut -d ' ' -f 1)
		if [ -n "$numBlocks" ]; then
			sum=$(($numBlocks + $sum))
		else
			# see if we are at end, otherwise assume an error occured
			grep -E '0 bytes copied|cpio:\s+premature end of archive' cpio.out
			rc=$?
			[ $rc -ne 0 ] && { stderr "failed while expanding initrd"; return 1; }
		fi
	done

	# Now swap out manifestCA.pem
	sudo cp "$keysetPath/manifest-ca/cert.pem" "manifestCA.pem" || {
        stderr "failed to cp manifestCA"
        return 1
    }

	# pack into new initrd
    find . | sudo cpio -o -H newc | gzip > ../new-initrd.gz || {
        stderr "failed to create a new initrd"
        return 1
    }

	# copy initrd back into PE
	objcopy --update-section .initrd=new-initrd.gz $uki || {
		stderr "failed to update .initrd section of PE"
		return 1
	}

	# re-sign the kernel.efi using new keyset
	sbsign --key $keysetPath/uki-production/privkey.pem --cert $keysetPath/uki-production/cert.pem --output $keysetPath/manifest/$product/bootkit/$uki $uki || {
		stderr "failed to re-sign the kernel"
		return 1
	}
	return 0
}

doshim() {
	local keyset=$1
	local output=$2

	cd bootkit && KEYSET=$1 BUILD_D=$WORKSPACE make shim || {
		stderr "failed to build shim"
		return 1
	}
	# sign the new shim
	sbsign --key $keyset/uefi-db/privkey.pem --cert $keyset/uefi-db/cert.pem --output $output shimx64.efi || {
		stderr "failed to sign the shim"
		return 1
	}
	
	# copy the shim to the keyset's bootkit

	return 0
}	

doovmf() {
	local keyset=$1
	local output=$2
	
	cd bootkit && KEYSET=$1 BUILD_D=$WORKSPACE make ovmf || {
		stderr "failed to build shim"
		return 1
	}
	
	# copy the artifacts to the keyset's bootkit.

}
		
main() {
	local keysetName=$1
	local keysetPath=$2
	local productName=$3
	local kernel=""
	local bootkitPath=$keysetPath/manifest/$productName/bootkit

	# create bootkit directory in keyset
	mkdir $bootkitPath || {
		stderr "failed to create bootkit dir in $keysetName for $productName"
		return 1
	}

	# create some temporary workspace
	WORKSPACE=$(mktemp -d --tmpdir=.) || {
		stderr "failed to create temp workspace"
		return 1
	}
	trap cleanup EXIT
	
	# git clone bootkit to generate shim and ovmf with new keyset
	#git clone https:/github.com/project-machine/bootkit.git $WORKSPACE || {
	#	stderr "failed to git clone bootkit"
	#	return 1
	#}

	# skopeo copy latest bootkit to get initrd
	skopeo copy "${BOOTKIT_URL}" ${WORKSPACE}/oci:bootkit:latest || {
		stderr "failed to skopeo copy bootkit"
		return 1
	}
	
	sudo atomfs mount oci:bootkit:latest mnt || {
		stderr "failed to atomfs mount bootkit"
		return 1
	}

	cp mnt/bootkit/kernel.efi ${WORKSPACE} || {
		stderr "failed to copy kernel to worksapce"
		return 1
	}

	kernel=${WORKSPACE}/kernel.efi
	do_initrd "$kernel" "$keysetPath" "$productName"
}
