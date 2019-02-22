#!/bin/bash

if [ "$#" -eq 0 ]; then
    native=1
fi

while [[ "$#" > 0 ]]; do case $1 in
  # -m|--darwin) deploy="$2"; shift;;
  -g|--libgit) libgit=1;;
  -d|--docker) docker=1;;
  -n|--native) native=1;;
  -c|--copy) copy=1;;
  *) echo "Unknown parameter passed: $1"; exit 1;;
esac; shift; done

echo Running gofmt
gofmt -s -w .

echo Building:
[[ -n $libgit  ]] && echo   - libgit2
[[ -n $docker  ]] && echo   - docker
[[ -n $native  ]] && echo   - native


function build_native {
    mkdir -p build/native
    cd swarm/cmd
    GO111MODULE=on go build --tags "static" -ldflags "-s -w" -o main ./*.go
    mv main ../../build/native/conscience-node
    cd -

    # mkdir -p build/native
    # cd remote-helper
    # GO111MODULE=on go build --tags "static" -ldflags "-s -w" -o main ./*.go
    # mv main ../build/native/git-remote-conscience
    # cd -

    # mkdir -p build/native
    # cd filters/encode
    # GO111MODULE=on go build --tags "static" -ldflags "-s -w" -o main ./*.go
    # mv main ../../build/native/conscience_encode
    # cd -

    # mkdir -p build/native
    # cd filters/decode
    # GO111MODULE=on go build --tags "static" -ldflags "-s -w" -o main ./*.go
    # mv main ../../build/native/conscience_decode
    # cd -

    # mkdir -p build/native
    # cd filters/diff
    # GO111MODULE=on go build --tags "static" -ldflags "-s -w" -o main ./*.go
    # mv main ../../build/native/conscience_diff
    # cd -

    # mkdir -p build/native
    # cd cmd
    # GO111MODULE=on go build --tags "static" -ldflags "-s -w" -o main ./*.go
    # mv main ../build/native/conscience
    # cd -
}

function build_docker {
    rm -rf ./build/docker &&
    mkdir -p build/docker &&
    pushd swarm/cmd &&
    GO111MODULE=on go build --tags "static" -o main ./*.go &&
    mv main ../../build/docker/conscience-node &&
    popd
}

function checkout_libgit2 {
    [[ -d vendor/github.com/libgit2/git2go ]] ||
        (mkdir -p vendor/github.com/libgit2 &&
        pushd vendor/github.com/libgit2 &&
        git clone https://github.com/Conscience/git2go &&
        pushd git2go &&
        git checkout f522924e75476de6dabb9c1bd8a5b22847959e24 &&
        # git remote add lhchavez https://github.com/lhchavez/git2go &&
        # git fetch --all &&
        # git cherry-pick 122ccfadea1e219c819adf1e62534f0b869d82a3 &&
        touch go.mod &&
        git submodule update --init &&
        popd && popd)
}

function build_libgit2 {
    checkout_libgit2

    pushd vendor/github.com/libgit2/git2go/vendor/libgit2 &&
    mkdir -p install/lib &&
    mkdir -p build &&
    pushd build &&

    cmake -DTHREADSAFE=ON \
      -DBUILD_CLAR=OFF \
      -DBUILD_SHARED_LIBS=OFF \
      -DCMAKE_C_FLAGS=-fPIC \
      -DUSE_SSH=OFF \
      -DCURL=OFF \
      -DUSE_HTTPS=OFF \
      -DUSE_BUNDLED_ZLIB=ON \
      -DUSE_EXT_HTTP_PARSER=OFF \
      -DCMAKE_BUILD_TYPE="RelWithDebInfo" \
      -DCMAKE_INSTALL_PREFIX=../install \
      .. && \
    cmake --build . &&
    popd && popd &&
    cat <<EOF

libgit2: Build complete.

  ===================================================================================
  | If you've just rebuilt libgit2 and are expecting git2go to pick up your changes |
  | next time you compile, please note that you'll need to use go build's "-a" flag |
  | like so:                                                                        |
  |                                                                                 |
  |    go build -a --tags "static" .                                                |
  |                                                                                 |
  | Subsequent builds will not require this flag unless you modify libgit2 again.   |
  ===================================================================================

EOF
}

[[ -n $libgit ]] && build_libgit2
[[ -n $docker ]] && build_docker
[[ -n $native ]] && build_native

[[ -n $copy ]] && cp -R ./build/* $DESKTOP_APP_BINARY_ROOT/

echo Done.
