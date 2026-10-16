# Contributing to Nexo

Thank you for your interest in Nexo. Bug reports, ideas and pull requests are welcome in the
repository <https://github.com/R3n3r0/Nexo>.

## Before opening a pull request

- Follow the conventions of the code base (see the README, section "Sviluppo", and
  [docs/12](docs/12-architettura-servizi.md)); keep each change focused.
- Run the checks that cover your change: `make lint`, `make test`, and for the console
  `make web-lint` and `make web-test`. Update the documentation, `CHANGELOG.md` and the
  traceability matrix (`make traceability`) when the behaviour changes.

## Licensing of contributions (contributor agreement)

Nexo is distributed under the GNU AGPL v3.0 only and is also offered under commercial licenses by
its copyright holder (see [COMMERCIAL-LICENSE.md](COMMERCIAL-LICENSE.md)). Dual licensing is only
possible if the copyright holder can license every part of the code under both. Therefore, by
submitting a contribution (code, documentation or any other material) you agree that:

1. the contribution is your original work, or you otherwise have the right to submit it under
   these terms;
2. the contribution is licensed to the public under the GNU AGPL v3.0 only, like the rest of Nexo;
3. you grant the copyright holder of Nexo (R3n3r0) and its successors a perpetual, worldwide,
   non-exclusive, royalty-free, irrevocable license to use, modify, sublicense and distribute your
   contribution under any license, including the AGPL and proprietary commercial licenses;
4. you keep the copyright of your contribution and may use it in any other way.

Confirm your agreement by signing off every commit (`git commit -s`), which adds the line

    Signed-off-by: Your Name <your.address@example.com>

The sign-off certifies the [Developer Certificate of Origin 1.1](https://developercertificate.org/)
and your acceptance of the terms above. Pull requests whose commits are not signed off cannot be
merged.
