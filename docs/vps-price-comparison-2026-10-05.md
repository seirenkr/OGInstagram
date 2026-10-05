# Vultr · OVHcloud VPS 요금 비교

확인일: 2026-10-05, 한국 시간. 공식 상품 페이지와 인증 없이 조회 가능한 공식 API를 기준으로 비교했으며 Vultr `ewr`의 공개 재고도 확인했다. 실제 계정, 청구서, 이용량과 계정별 주문 가능 여부는 확인하지 않았다.

최종 선택은 **Vultr High Frequency `vhf-1c-1gb` / New Jersey(`ewr`) / Ubuntu 24.04 LTS 한 대**다. 기본 월요금은 $6 세전, 한국 개인 VAT 10% 적용 시 예상 $6.60이며 자동백업은 선택하지 않는다. 일반형 $5와 High Performance AMD $6는 비교 옵션이다. 계정 가입은 사용자가 직접 진행하고 Docker 구성은 일반 VPS에서 사용할 수 있는 형태를 유지한다.

## 월 무약정 운영비

USD 기준이며 세금, 카드 환전 비용, 별도 외부 백업 저장비는 제외한다.

| 상품 | CPU / RAM | 디스크 | 월 포함 전송량 | 기본 월요금 | 자동백업 포함 월요금 |
| --- | --- | --- | --- | ---: | ---: |
| Vultr Regular `vc2-1c-1gb` | 공유 1 vCPU / 1,024 MB | 25 GB SSD | 1,024 GB | $5.00 | $6.00 |
| **선택: Vultr High Frequency `vhf-1c-1gb`** | **공유 1 vCPU / 1,024 MB** | **32 GB NVMe** | **1,024 GB** | **$6.00** | **$7.20** |
| Vultr High Performance AMD `vhp-1c-1gb-amd` | 공유 1 vCPU / 1,024 MB | 25 GB NVMe | 2,048 GB | $6.00 | $7.20 |
| OVHcloud VPS-1 2027 `vps-2027-model1` | 2 vCores / 4 GB | 40 GB NVMe | 미국 지역 무제한, 최대 500 Mbps | $5.35 | $5.35, 표준 백업 포함 |
| OVHcloud VPS-1 + Premium Backup | 위와 동일 | 위와 동일 | 위와 동일 | $5.35 | $6.75 |

Vultr는 [공식 계획 API](https://api.vultr.com/v2/plans)의 `id`, `vcpu_count`, `ram`, `disk`, `disk_type`, `bandwidth`, `monthly_cost`를 확인했다. New Jersey(`ewr`)에 별도 지역 할증은 표시되지 않았다. 자동백업은 기본요금의 20%를 더한다. [Vultr 자동백업](https://docs.vultr.com/vps-automatic-backups)

**현재 기본안은 자동백업을 선택하지 않아 $6 세전·예상 $6.60 세후**다. 자동백업 선택 시 $7.20 세전·예상 $7.92 세후이며, Regular는 $5 세전·예상 $5.50 세후, 백업 선택 시 $6·$6.60이다. 한국 VAT 10%는 공식 세금 표에 따른 예상이며 청구 주소·면세 여부·최종 청구를 확인한다. [Vultr 세금 안내](https://docs.vultr.com/support/platform/billing/does-vultr-collect-vat-or-sales-tax)

Vultr 신용카드 등록은 $0 deposit 옵션이 있어 최초 충전 없이 진행할 수 있다. 일시적인 카드 승인 보류가 발생할 수 있고 다른 결제 수단에는 deposit이 필요할 수 있다. 서버는 시간 단위로 사용액을 누적하고 익월 1일에 전월 청구서를 만들며, 정지만으로 과금이 끝나지는 않는다. [가입 공식 안내](https://docs.vultr.com/platform/create-an-account), [후불 청구 방식](https://docs.vultr.com/support/platform/billing/how-am-i-billed-for-my-servers)

OVHcloud는 [공식 US 카탈로그 API](https://api.us.ovhcloud.com/1.0/order/catalog/public/vps?ovhSubsidiary=US)의 `vps-2027-model1`에서 `mode=default`, `commitment=0`, `interval=1`, `intervalUnit=month`, `formattedPrice=$5.35 USD`를 확인했다. Linux와 기본 로컬 디스크 옵션은 $0이다. CPU·RAM·디스크·회선·IPv4는 [공식 US 상품 페이지](https://us.ovhcloud.com/vps/)에서 확인했다.

## OVHcloud 선결제 가격은 별도로 비교

| 결제 조건 | 월 환산 가격 | 해당 기간 선결제 합계 |
| --- | ---: | ---: |
| 무약정, 매월 결제 | $5.35 | 매월 $5.35 |
| 6개월 선결제 | $5.08 | $30.48 |
| 12개월 선결제 | $4.54 | $54.48 |

공식 홈페이지의 **“$4.54/월부터”는 12개월 선결제 가격**이다. [상품 페이지의 Configure 링크](https://us.ovhcloud.com/vps/configurator/?brick=VPS%2BModel%2B1&planCode=vps-2027-model1&pricing=upfront12&processor=+&storage=40__SSD__NVMe&vcore=2__vCore)에도 `pricing=upfront12`가 지정되어 있다. 카탈로그의 `upfront6`/`upfront12`에는 각각 6/12개월 계약이 표시되므로 Vultr의 월 가격과 비교할 때는 $5.35를 사용한다. 할인 계약의 갱신 조건은 주문 시 확인한다.

## 백업과 전송비 차이

| 항목 | Vultr 자동백업 | OVHcloud 표준 백업 | OVHcloud Premium Backup |
| --- | --- | --- | --- |
| 추가요금 | 기본요금 +20% | 현재 포함 | VPS-1 기준 $1.40/월 |
| 보관 | 최근 2개 | 매일 백업, 24시간 보관 | 최근 7일의 일별 복구점 |
| 데이터 범위 | 서버 파일시스템, 별도 Block Storage 제외 | 기본 VPS 디스크 | 기본 VPS 디스크, 추가 디스크 제외 |

OVHcloud 표준 백업은 공식 페이지에서 포함 기능으로 명시한다. API의 `option-auto-backup-2027-1-model1`에는 기본 $0.50와 100% 무료 혜택, 할인 후 `total.formattedValue=$0.00 USD`, `endDate=null`이 표시되어 있다. Premium 옵션 `option-auto-backup-2027-7-model1`은 월 $1.40이다. 실제 주문에서 표준 백업 할인 적용과 최종 합계를 확인한다. [OVHcloud 백업 옵션](https://us.ovhcloud.com/vps/options/)

사업자 백업만으로 SQLite의 일관된 외부 백업을 대신하지 않는다. 현재 기본안은 유료 Vultr 자동백업을 선택하지 않고 앱의 `--backup` snapshot과 암호화된 VM 외부 보관, 소진 처리 후 복구 검증을 유지한다. Vultr 자동백업과 OVHcloud Premium 백업은 원본과 같은 데이터센터에 보관된다. [Vultr 백업 범위·제한](https://docs.vultr.com/vps-automatic-backups), [OVHcloud 백업 보관 방식](https://us.ovhcloud.com/vps/options/)

Vultr는 outbound만 집계하며 포함량 초과는 $0.01/GB다. Cloudflare로 보내는 응답과 외부 백업 전송도 outbound이므로 CDN 또는 Tunnel 유지가 원본 전송비를 없애지는 않는다. [집계 기준](https://docs.vultr.com/support/platform/billing/how-is-bandwidth-usage-calculated), [초과요금](https://docs.vultr.com/support/platform/billing/what-is-the-bandwidth-overage-rate)

OVHcloud 표의 무제한 조건은 **미국 일반 데이터센터 VPS** 기준이다. 아시아·태평양 상품은 월 전송량 제한이 있으며 Local Zone은 디스크와 지원 옵션이 다르다. [미국 상품 조건과 지역별 예외](https://us.ovhcloud.com/vps/)

## 이 앱에서의 선택 기준

Cloudflare Container Lite는 256 MiB RAM, Basic은 1 GiB RAM이다. Vultr 1 GB VM에서 앱은 1 CPU·512 MiB, cloudflared는 128 MiB 제한을 유지하고 OS·Docker 메모리를 남긴다. 단일 VM에서 수집 부하·OOM을 확인하며 자동 확장이나 추가 서버를 구성하지 않는다. [Cloudflare 공식 인스턴스 규격](https://developers.cloudflare.com/containers/platform/limits/)

High Frequency는 일반형보다 월 $1 높은 가격에 3 GHz 초과 CPU와 NVMe를 제공한다. 사용자 선호에 따라 HF를 선택했으며, 이 앱에서 같은 $6의 High Performance AMD보다 빠르다는 벤치마크는 수행하지 않았다. HF는 공유 Intel CPU이며 월 전송량은 1,024 GB로 HP AMD의 2,048 GB보다 적다. [Vultr 상품 설명](https://docs.vultr.com/products/compute/instances/cloud-compute/provisioning), [공식 계획 API](https://api.vultr.com/v2/plans)

상시 운영 기준으로 OVHcloud의 최저 일반 VPS는 4 GB지만 가격은 무약정 $5.35다. 이는 이 앱에 4 GB가 필요하다는 뜻이 아니라, **비슷한 월 예산에 더 큰 기본 상품을 제공한다는 의미**다. 기본 백업 포함 시 Vultr Regular보다 $0.65, High Performance보다 $1.85/월 저렴하다. 백업 보관 정책과 실제 CPU 성능은 동일하지 않으므로 가격표만으로 성능 우위를 확정할 수 없다.

미국 동부에서는 Vultr New Jersey(`ewr`)를 선택했으며 2026-10-05 API에서 HF 1 GB 재고를 확인했다. OVHcloud Virginia(`US-EAST-VA`)는 **별도 US 계정의 US 카탈로그**에 표시되는 지역이다. US와 CA/ASIA/WE 계정·주문 경로를 같은 것으로 보지 않는다. [Vultr 재고](https://api.vultr.com/v2/regions/ewr/availability), [OVHcloud US 주문 카탈로그](https://api.us.ovhcloud.com/1.0/order/catalog/public/vps?ovhSubsidiary=US)

현재 CA 주문 API의 VPS-1에는 **CA·ASIA·WE 모두 미국 리전이 포함되지 않는다**. ASIA/WE에서도 무약정 $5.35 가격을 확인했지만 해당 주문의 데이터센터 목록에는 `US-EAST-VA`·`US-WEST-OR`가 없으므로 같은 계정에서 미국 VPS를 주문할 수 있다는 뜻이 아니다. 미국 사용에는 별도 US 가입·주문 조건을 확인해야 한다. [CA 카탈로그](https://ca.api.ovh.com/1.0/order/catalog/public/vps?ovhSubsidiary=CA), [ASIA 카탈로그](https://ca.api.ovh.com/1.0/order/catalog/public/vps?ovhSubsidiary=ASIA), [WE 카탈로그](https://ca.api.ovh.com/1.0/order/catalog/public/vps?ovhSubsidiary=WE)

세금과 가입·주문 조건은 계약 법인 및 계정 국가에 따라 달라진다. OVHcloud US 카탈로그의 `taxRate=0`을 한국 사용자의 최종 세금 면제로 해석하지 않는다. 가격·지역·할인·세금은 결제 직전 다시 확인한다.
