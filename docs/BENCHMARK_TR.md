# PureSend — Performans Ölçümleri

[English](BENCHMARK.md)

Bu belgedeki her sayı ölçüldü ve komutuyla birlikte verildi; aynı makinede tekrar çalıştırılabilir. Ölçülmemiş olanlar en sonda ayrıca listelendi.

> **Donanım ve ortam:** AMD Ryzen 7 5700X (8 çekirdek / 16 iş parçacığı), 32 GB RAM, NVMe SSD, Linux 6.8, Go 1.27.1; başka programların çalıştığı bir masaüstü. Ölçülen kod v2.0.7 (`7024fdf`), ölçüm tarihi 2026-10-01.

---

## 1. Özet

| Ölçüm | Sonuç | Kaynak |
| :--- | :---: | :--- |
| Uçtan uca aktarım hızı (loopback) | **270–440 MB/s** | `make bench-e2e`, 256 MiB – 8 GiB |
| Tepe bellek (RSS), gönderici / alıcı | **36–39 MB / 33–36 MB** | aynı ölçüm, 256 MiB – 8 GiB |
| El sıkışma, iki tarafın CPU işi | **~0,65 ms** | `BenchmarkHandshake` |
| Dilim sıkıştırma / açma | **~800 MB/s / ~225 MB/s** | `BenchmarkCompressChunk`, `BenchmarkDecompressChunk` |

**Nasıl okunmalı:** Loopback'in kendi hat hızı yoktur. Bu yüzden uçtan uca ölçüm ağı değil, yazılımın koyduğu tavanı gösterir: şifreli libp2p bağlantısı, iki uçta SHA-256, diske yazma ve protokolün kendisi. En yavaş çalıştırma bile (269 MB/s), gigabit Ethernet'in pratik tavanının (~118 MB/s) iki katından fazladır. Yani bu donanımda yerel ağdaki darboğaz ağın kendisi olur. Daha yavaş bir işlemci ya da diskte bu tavan düşer. Çalıştırmalar arasındaki fark büyüktür (bölüm 2); bu yüzden tek bir sayı değil bir aralık verilir.

---

## 2. Uçtan uca ölçüm (`make bench-e2e`)

[`scripts/bench-e2e.sh`](../scripts/bench-e2e.sh) programı bir kullanıcının çalıştırdığı gibi ölçer:

1. Yerel bir buluşma sunucusu başlatır (`puresend-server`, TCP, loopback).
2. `/dev/urandom`'dan istenen boyutta bir dosya üretir. Rastgele veri sıkışmadığı için her bayt bağlantıdan geçer.
3. Göndericiyi başlatır ve dosyaları okuyup özetini çıkarmasını bekler (`files ready`). Böylece ön hazırlık süresi ölçüme girmez.
4. Alıcıyı başlatır ve süresini ölçer: başlangıç, oda sorgusu, bağlantı, el sıkışma, aktarım ve doğrulama dahil.
5. Bağlantının doğrudan kurulduğunu ve iki dosyanın SHA-256 özetlerinin aynı olduğunu kontrol eder.
6. İki sürecin tepe belleğini GNU `time` ile raporlar.

Sonuçlar (her satır ayrı bir çalıştırma):

| Veri | Alıcı süresi | Hız | Tepe RSS, gönderici | Tepe RSS, alıcı |
| :---: | :---: | :---: | :---: | :---: |
| 256 MiB | 0,99 s | 271 MB/s | 36 MB | 33 MB |
| 2 GiB | 6,70 s | 320 MB/s | 38 MB | 34 MB |
| 2 GiB | 5,72 s | 375 MB/s | 39 MB | 35 MB |
| 2 GiB | 6,53 s | 329 MB/s | 38 MB | 35 MB |
| 8 GiB | 31,93 s | 269 MB/s | 38 MB | 36 MB |

- **Bellek dosya boyutuyla büyümüyor.** 32 kat büyük dosyada tepe RSS birkaç MB oynuyor. Dosyalar 32 KB'lık dilimlerle okunup yazılıyor ve hiçbir zaman belleğe bütün olarak alınmıyor.
- **Çalıştırmalar arasındaki fark büyük ve bu kodun değil, makinenin farkı.** Aynı gün yapılan ikinci bir seri v2.0.7'de 272–437 MB/s verdi (2 GiB: 416, 401, 309; 8 GiB: 437; 256 MiB: 272). v2.0.0'dan (`27ff753`) derlenen ikili dosyalar, v2.0.7'ninkilerle sırayla çalıştırıldığında 302–458 MB/s verdi; yani iki sürüm arasında bir gerileme yok. Farkın nedenini ayrıştırmadık; şüpheliler diğer süreçler ile sayfa önbelleğinin ve diske yazma durumunun o anki hali.
- **256 MiB'da hız genellikle daha düşük**, çünkü sürenin sabit kısmı (süreç başlangıcı, sunucu bağlantısı, el sıkışma) kısa bir aktarımda oransal olarak daha büyük.
- Alıcı veriyi işletim sisteminin sayfa önbelleğine yazıyor. Diske gerçekten yazılma hızı ayrı bir konudur.

```bash
make bench-e2e                       # 2 GiB
FT_BENCH_SIZE_MB=8192 make bench-e2e # 8 GiB
```

---

## 3. Mikro-benchmark'lar

Bunlar tek bir fonksiyonu ölçer. Asıl işleri gerilemeleri yakalamaktır; kullanıcının hissettiği hızı uçtan uca ölçüm gösterir.

```bash
make bench   # go test -run '^$' -bench . -benchmem ./...
```

Üç çalıştırmanın aralığı:

| Benchmark | Süre | Hız | Bellek | Tahsis |
| :--- | :---: | :---: | :---: | :---: |
| `BenchmarkCompressChunk` | 39–40 µs | 792–812 MB/s | ~58 B | 1 |
| `BenchmarkDecompressChunk` | 140–146 µs | 219–228 MB/s | ~125 B | 3 |
| `BenchmarkHandshake` | 0,63–0,68 ms | — | ~30 KB | 373–374 |
| `BenchmarkSafeJoin` | 621–808 ns | — | 240 B | 5 |
| `BenchmarkValidDigest` | 25–26 ns | — | 0 B | 0 |
| `BenchmarkClean_CleanText` | 705–803 ns | — | 120 B | 4 |
| `BenchmarkClean_UnsafeText` | 384–414 ns | — | 56 B | 3 |

- **Sıkıştırma** metin benzeri bir 32 KB dilim üzerinde ölçülür. Sıkıştırma `flate.HuffmanOnly` ile yapılır, çünkü eşleşme aramayan bu mod hat hızının üzerinde çalışır. Küçülmeyen dilim (JPEG, MP4, ZIP gibi) olduğu gibi gönderilir, yani sıkışmayan veride bağlantı üzerinden hiçbir şey büyümez.
- **El sıkışma** iki tarafın PAKE ve karşılıklı onay adımlarını bellekteki bir boru üzerinden birlikte ölçer. Gerçek bir bağlantıda buna ağın bir iki gidiş-dönüşü eklenir ve süreyi o belirler.
- **Güvenlik denetimleri** (`SafeJoin`, `Clean`) mikro saniyenin altındadır ve dosya başına bir kez çalışır, bayt başına değil. Aktarım hızına etkileri ölçülemeyecek kadar küçüktür.

---

## 4. Henüz ölçülmeyenler

- **Gerçek ağ üzerinde LAN hızı:** Loopback tavanı 270–440 MB/s. İki makine arasında gigabit ya da 2,5G bir bağlantıda ölçülmedi.
- **İnternet üzerinden hız ve delik açma başarı oranı:** Farklı ev, mobil (CGNAT) ve kurumsal ağ çiftlerinde DCUtR'nin ne sıklıkla doğrudan bağlantı kurabildiği ölçülmedi.
- **Röle üzerinden hız:** Röle yedeği [`test/relay`](../test/relay/netns-relay-test.sh) ile her commit'te doğruluk açısından test ediliyor, ama hız ölçümü yapılmıyor. Üretimdeki sunucu röle bağlantısı başına veri ve süre sınırı koyar (`-relay-data`, `-relay-duration`).
