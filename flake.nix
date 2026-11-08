{
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = {nixpkgs, ...}: let
    systems = [
      "x86_64-linux"
      "aarch64-darwin"
    ];
    perSystem = nixpkgs.lib.genAttrs systems (system: let
      pkgs = nixpkgs.legacyPackages.${system};

      basicTools =
        [
          pkgs.bash
          pkgs.coreutils
          pkgs.findutils
          pkgs.gnugrep
          pkgs.gnused
          pkgs.gawk
          pkgs.diffutils
          pkgs.patch
          pkgs.git
          pkgs.ripgrep
          pkgs.jq
          pkgs.curl
        ]
        ++ pkgs.lib.optional pkgs.stdenv.hostPlatform.isLinux pkgs.bubblewrap;

      ppq-private-proxy = pkgs.buildNpmPackage {
        pname = "ppq-private-mode";
        version = "0.7.0";

        src = pkgs.fetchFromGitHub {
          owner = "PayPerQ";
          repo = "ppq-privacy-verifier";
          rev = "d5a78c8734b37b4763b4579ad2207d95a08fa5d3";
          hash = "sha256-PDmQ1CKtT0EnNRwdxeoiBZfHlmDDQFJ5YIt7eJgH5PE=";
        };

        npmDepsHash = "sha256-cCnL8mf1Z0PM+F0zr08HX647aKidMGXo7IPuNhlHJDM=";
        npmInstallFlags = ["--omit=peer"];
      };

      a = pkgs.buildGoModule {
        pname = "a";
        version = "0-unstable";
        src = ./.;

        subPackages = [
          "."
          "cmd/a-chatgpt-proxy"
        ];
        env.CGO_ENABLED = "0";
        vendorHash = null;
        doCheck = false;

        nativeBuildInputs = [pkgs.makeWrapper];
        postInstall = ''
          wrapProgram $out/bin/a \
            --prefix PATH : ${pkgs.lib.makeBinPath basicTools}
        '';
      };
    in {
      packages = {
        default = a;
        inherit a;
      };

      apps = {
        default = {
          type = "app";
          program = "${a}/bin/a";
        };
        a = {
          type = "app";
          program = "${a}/bin/a";
        };
        a-chatgpt-proxy = {
          type = "app";
          program = "${a}/bin/a-chatgpt-proxy";
        };
        ppq-private-proxy = {
          type = "app";
          program = "${ppq-private-proxy}/bin/ppq-private-proxy";
        };
      };

      devShells.default = pkgs.mkShellNoCC {
        CGO_ENABLED = "0";

        packages =
          basicTools
          ++ [
            pkgs.go
            pkgs.llama-cpp
            ppq-private-proxy
          ];

        LLAMA_ARG_MODEL = pkgs.fetchurl {
          url = "https://huggingface.co/Qwen/Qwen3-4B-GGUF/resolve/bc640142c66e1fdd12af0bd68f40445458f3869b/Qwen3-4B-Q4_K_M.gguf";
          hash = "sha256-dIX+bxGvKUM7xRyrWACVIfIFhA9bSuOjL6f5LoU0/fU=";
        };
      };
    });
  in {
    packages = builtins.mapAttrs (_: config: config.packages) perSystem;
    apps = builtins.mapAttrs (_: config: config.apps) perSystem;
    devShells = builtins.mapAttrs (_: config: config.devShells) perSystem;
  };
}
