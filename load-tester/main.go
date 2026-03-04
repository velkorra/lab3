package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	vegeta "github.com/tsenart/vegeta/v12/lib"
)

// countingTransport считает количество активных запросов.
type countingTransport struct {
	active int32
	base   http.RoundTripper
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	atomic.AddInt32(&t.active, 1)
	defer atomic.AddInt32(&t.active, -1)
	return t.base.RoundTrip(req)
}

func (t *countingTransport) Active() int32 {
	return atomic.LoadInt32(&t.active)
}

// TimeMetrics хранит информацию об одном запросе.
type TimeMetrics struct {
	StartTime  time.Time // когда запрос был отправлен
	EndTime    time.Time // когда получен ответ
	Latency    time.Duration
	StatusCode int
	Success    bool // 2xx
	BytesOut   uint64
	BytesIn    uint64
	Sequence   uint64
	Error      string
}

// IntervalStats — статистика за временной интервал (по времени отправки).
type IntervalStats struct {
	StartTime       time.Time
	EndTime         time.Time
	TotalRequests   int
	SuccessRequests int
	ErrorRequests   int
	// Статистика только по успешным запросам (2xx)
	SuccessAvgLatency time.Duration
	SuccessMaxLatency time.Duration
	SuccessMinLatency time.Duration
	SuccessP95Latency time.Duration
	SuccessP99Latency time.Duration
	// Общая пропускная способность (все запросы)
	TotalRPS float64
	// RPS только успешных
	SuccessRPS float64
}

func main() {
	var (
		url         string
		targetRPS   int
		duration    time.Duration
		interval    time.Duration
		detailed    bool
		timeout     time.Duration
		method      string
		bodyFile    string
		headers     string
		successOnly bool // новый флаг
	)
	detailed = true
	flag.StringVar(&url, "url", "https://app-lab3.hakurei.dev/work", "URL для тестирования")
	flag.IntVar(&targetRPS, "rps", 480, "Целевое количество запросов в секунду")
	flag.DurationVar(&duration, "duration", 500*time.Second, "Длительность теста")
	flag.DurationVar(&interval, "interval", 1*time.Second, "Интервал детальной статистики")
	flag.DurationVar(&timeout, "timeout", 30*time.Second, "Таймаут запроса")
	flag.StringVar(&method, "method", "GET", "HTTP метод")
	flag.StringVar(&bodyFile, "body", "", "Файл с телом запроса")
	flag.StringVar(&headers, "headers", "", "Заголовки (Key: Value, Key2: Value2)")
	flag.BoolVar(&successOnly, "success-only", false, "Учитывать только успешные (2xx) запросы в статистике latency")
	flag.Parse()

	if targetRPS <= 0 || duration <= 0 {
		log.Fatal("RPS и duration должны быть положительными")
	}

	fmt.Printf("Запуск нагрузочного тестирования:\n")
	fmt.Printf("URL: %s\n", url)
	fmt.Printf("Целевой RPS: %d\n", targetRPS)
	fmt.Printf("Длительность: %v\n", duration)
	fmt.Printf("Интервал статистики: %v\n", interval)
	fmt.Printf("Таймаут: %v\n", timeout)
	fmt.Printf("Метод: %s\n", method)
	fmt.Printf("Учитывать только успешные: %v\n", successOnly)
	fmt.Printf("Детальный режим: %v\n\n", detailed)

	rate := vegeta.Rate{Freq: targetRPS, Per: time.Second}

	// Формируем цель
	target := vegeta.Target{
		Method: method,
		URL:    url,
	}
	if headers != "" {
		target.Header = make(map[string][]string)
		for _, pair := range strings.Split(headers, ",") {
			parts := strings.SplitN(strings.TrimSpace(pair), ":", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				val := strings.TrimSpace(parts[1])
				target.Header[key] = []string{val}
			}
		}
	}
	if bodyFile != "" {
		body, err := os.ReadFile(bodyFile)
		if err != nil {
			log.Fatalf("Ошибка чтения тела запроса: %v", err)
		}
		target.Body = body
	}
	targeter := vegeta.NewStaticTargeter(target)

	// Создаём транспорт с подсчётом активных запросов
	transport := &countingTransport{base: http.DefaultTransport}
	client := &http.Client{Transport: transport}

	// Рассчитываем минимальное количество воркеров, чтобы поддерживать целевой RPS при максимальной ожидаемой задержке.
	// По умолчанию берём с запасом: workers = targetRPS * 2 (можно сделать параметром)
	workers := targetRPS * 4 // эмпирически, можно вынести в флаг
	attacker := vegeta.NewAttacker(
		vegeta.Timeout(timeout),
		vegeta.Workers(uint64(workers)),
		vegeta.MaxWorkers(uint64(workers*2)),
		vegeta.Client(client),
	)

	results := make(chan *vegeta.Result, 10000)
	timeMetrics := make([]TimeMetrics, 0)

	go func() {
		for res := range attacker.Attack(targeter, rate, duration, "Load Test") {
			results <- res
		}
		close(results)
	}()

	testStartTime := time.Now()
	var allMetrics vegeta.Metrics
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	requestTimes := make([]time.Time, 0) // для расчёта RPS в реальном времени

	for res := range results {
		allMetrics.Add(res)

		// Вычисляем время отправки запроса (приблизительно)
		endTime := time.Now()
		startTime := endTime.Add(-res.Latency)
		success := res.Code >= 200 && res.Code < 300

		timeMetrics = append(timeMetrics, TimeMetrics{
			StartTime:  startTime,
			EndTime:    endTime,
			Latency:    res.Latency,
			StatusCode: int(res.Code),
			Success:    success,
			BytesOut:   res.BytesOut,
			BytesIn:    res.BytesIn,
			Sequence:   res.Seq,
			Error:      res.Error,
		})

		requestTimes = append(requestTimes, endTime)

		select {
		case <-ticker.C:
			if detailed {
				elapsed := time.Since(testStartTime)
				progress := elapsed.Seconds() / duration.Seconds() * 100
				if progress <= 100 {
					// RPS за последнюю секунду (все запросы)
					now := time.Now()
					oneSecAgo := now.Add(-time.Second)
					recent := 0
					for _, t := range requestTimes {
						if t.After(oneSecAgo) {
							recent++
						}
					}

					active := transport.Active()
					barLen := int(progress / 5)
					if barLen > 20 {
						barLen = 20
					}
					// Подсчёт успешных за последнюю секунду
					recentSuccess := 0
					for i := len(timeMetrics) - 1; i >= 0 && timeMetrics[i].EndTime.After(oneSecAgo); i-- {
						if timeMetrics[i].Success {
							recentSuccess++
						}
					}
					fmt.Printf("\rПрогресс: [%-20s] %.1f%% | Запросов: %d | Успешно(за сек): %d | Ошибок(за сек): %d | Активных: %d | Текущий latency: %v | Реальный RPS: %3d (цель: %d)\n",
						strings.Repeat("=", barLen)+strings.Repeat(" ", 20-barLen),
						progress, len(timeMetrics), recentSuccess, recent-recentSuccess, active,
						res.Latency.Round(time.Millisecond), recent, targetRPS)
				}
			}
		default:
		}
	}
	allMetrics.Close()
	fmt.Println()

	// Вывод основной статистики
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("ОСНОВНЫЕ РЕЗУЛЬТАТЫ ТЕСТИРОВАНИЯ")
	fmt.Println(strings.Repeat("=", 80))
	printMainStats(&allMetrics, duration, testStartTime, targetRPS, timeMetrics, successOnly)

	var intervalStats []IntervalStats
	// Статистика по интервалам (группировка по времени отправки)
	if detailed {
		fmt.Println("\n" + strings.Repeat("=", 80))
		fmt.Println("СТАТИСТИКА ПО ВРЕМЕННЫМ ИНТЕРВАЛАМ (по времени отправки)")
		fmt.Println(strings.Repeat("=", 80))
		intervalStats := calculateIntervalStatsByStart(timeMetrics, interval, testStartTime, successOnly)
		printIntervalStats(intervalStats, targetRPS)

		if len(intervalStats) > 0 {
			fmt.Println("\n" + strings.Repeat("=", 80))
			fmt.Println("ГРАФИК ПРОИЗВОДИТЕЛЬНОСТИ")
			fmt.Println(strings.Repeat("=", 80))
			printPerformanceGraphs(intervalStats, targetRPS)
		}
	}

	// Детальная статистика по перцентилям (только успешные, если successOnly)
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("ДЕТАЛЬНАЯ СТАТИСТИКА ПО ПЕРЦЕНТИЛЯМ")
	fmt.Println(strings.Repeat("=", 80))
	printPercentileStats(&allMetrics, timeMetrics, successOnly)

	// Анализ производительности
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("АНАЛИЗ ПРОИЗВОДИТЕЛЬНОСТИ")
	fmt.Println(strings.Repeat("=", 80))
	printPerformanceAnalysis(&allMetrics, duration, targetRPS, timeMetrics)

	// Анализ ошибок
	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("АНАЛИЗ ОШИБОК")
	fmt.Println(strings.Repeat("=", 80))
	printErrorAnalysis(&allMetrics, timeMetrics)

	// Сохранение в файл
	saveToFile(&allMetrics, duration, intervalStats, timeMetrics, targetRPS, successOnly)
}

// --- Вспомогательные функции ---

// getSuccessCount возвращает количество успешных запросов (2xx)
func getSuccessCount(metrics *vegeta.Metrics) int {
	successCount := 0
	for code, count := range metrics.StatusCodes {
		var codeInt int
		fmt.Sscanf(code, "%d", &codeInt)
		if codeInt >= 200 && codeInt < 300 {
			successCount += count
		}
	}
	return successCount
}

// calculateAverage вычисляет среднее арифметическое длительностей
func calculateAverage(latencies []time.Duration) time.Duration {
	if len(latencies) == 0 {
		return 0
	}
	var total time.Duration
	for _, l := range latencies {
		total += l
	}
	return total / time.Duration(len(latencies))
}

// extractSuccessLatencies возвращает слайс длительностей только успешных запросов
func extractSuccessLatencies(metrics []TimeMetrics) []time.Duration {
	var latencies []time.Duration
	for _, m := range metrics {
		if m.Success {
			latencies = append(latencies, m.Latency)
		}
	}
	return latencies
}

// calculateIntervalStatsByStart группирует запросы по времени отправки
func calculateIntervalStatsByStart(metrics []TimeMetrics, interval time.Duration, testStartTime time.Time, successOnly bool) []IntervalStats {
	if len(metrics) == 0 {
		return nil
	}

	// Определяем границы интервалов от testStartTime до testStartTime+duration
	// Но metrics могут выходить за duration из-за долгих ответов, поэтому ограничим интервалы до max(EndTime)
	maxTime := testStartTime
	for _, m := range metrics {
		if m.EndTime.After(maxTime) {
			maxTime = m.EndTime
		}
	}
	// Округляем до ближайшего интервала вверх
	durationUntil := maxTime.Sub(testStartTime)
	intervalCount := int(durationUntil/interval) + 1

	// Инициализируем мапу интервалов
	intervalMap := make(map[int][]TimeMetrics)
	for i := 0; i <= intervalCount; i++ {
		intervalMap[i] = []TimeMetrics{}
	}

	// Распределяем запросы по интервалам на основе StartTime
	for _, m := range metrics {
		idx := int(m.StartTime.Sub(testStartTime) / interval)
		if idx < 0 {
			idx = 0
		}
		if idx > intervalCount {
			idx = intervalCount
		}
		intervalMap[idx] = append(intervalMap[idx], m)
	}

	// Сортируем индексы
	indices := make([]int, 0, len(intervalMap))
	for k := range intervalMap {
		indices = append(indices, k)
	}
	sort.Ints(indices)

	stats := make([]IntervalStats, 0)
	for _, idx := range indices {
		requests := intervalMap[idx]
		if len(requests) == 0 {
			continue
		}
		start := testStartTime.Add(time.Duration(idx) * interval)
		end := testStartTime.Add(time.Duration(idx+1) * interval)

		var successLatencies []time.Duration
		totalReq := len(requests)
		successReq := 0
		for _, r := range requests {
			if r.Success {
				successReq++
				successLatencies = append(successLatencies, r.Latency)
			}
		}
		errorReq := totalReq - successReq

		stat := IntervalStats{
			StartTime:       start,
			EndTime:         end,
			TotalRequests:   totalReq,
			SuccessRequests: successReq,
			ErrorRequests:   errorReq,
			TotalRPS:        float64(totalReq) / interval.Seconds(),
			SuccessRPS:      float64(successReq) / interval.Seconds(),
		}

		if len(successLatencies) > 0 {
			sort.Slice(successLatencies, func(i, j int) bool { return successLatencies[i] < successLatencies[j] })
			stat.SuccessAvgLatency = calculateAverage(successLatencies)
			stat.SuccessMinLatency = successLatencies[0]
			stat.SuccessMaxLatency = successLatencies[len(successLatencies)-1]

			p95Idx := len(successLatencies) * 95 / 100
			if p95Idx >= len(successLatencies) {
				p95Idx = len(successLatencies) - 1
			}
			stat.SuccessP95Latency = successLatencies[p95Idx]

			p99Idx := len(successLatencies) * 99 / 100
			if p99Idx >= len(successLatencies) {
				p99Idx = len(successLatencies) - 1
			}
			stat.SuccessP99Latency = successLatencies[p99Idx]
		}

		stats = append(stats, stat)
	}
	return stats
}

// printMainStats выводит общую статистику с учётом successOnly
func printMainStats(metrics *vegeta.Metrics, duration time.Duration, startTime time.Time, targetRPS int, timeMetrics []TimeMetrics, successOnly bool) {
	actualRPS := float64(metrics.Requests) / duration.Seconds()
	achievedPercent := (actualRPS / float64(targetRPS)) * 100
	successCount := getSuccessCount(metrics)
	successRPS := float64(successCount) / duration.Seconds()

	fmt.Printf("Общая информация:\n")
	fmt.Printf("  Время начала теста: %s\n", startTime.Format("15:04:05.000"))
	fmt.Printf("  Время окончания теста: %s\n", time.Now().Format("15:04:05.000"))
	fmt.Printf("  Общая длительность: %v\n", duration)
	fmt.Printf("  Всего запросов: %d\n", metrics.Requests)
	fmt.Printf("  Успешных запросов (2xx): %d\n", successCount)
	fmt.Printf("  Успешность: %.2f%%\n", metrics.Success*100)
	fmt.Printf("\nСтатистика RPS:\n")
	fmt.Printf("  Целевой RPS: %d\n", targetRPS)
	fmt.Printf("  Реальный общий RPS: %.2f requests/sec\n", actualRPS)
	fmt.Printf("  Реальный успешный RPS: %.2f requests/sec\n", successRPS)
	fmt.Printf("  Достигнуто (общее): %.1f%% от цели\n", achievedPercent)

	// Статистика времени ответа: если successOnly, то только по успешным
	if successOnly {
		successLatencies := extractSuccessLatencies(timeMetrics)
		if len(successLatencies) > 0 {
			sort.Slice(successLatencies, func(i, j int) bool { return successLatencies[i] < successLatencies[j] })
			avg := calculateAverage(successLatencies)
			p50 := successLatencies[len(successLatencies)*50/100]
			p95 := successLatencies[len(successLatencies)*95/100]
			p99 := successLatencies[len(successLatencies)*99/100]
			max := successLatencies[len(successLatencies)-1]
			min := successLatencies[0]

			fmt.Printf("\nСтатистика времени ответа (только успешные):\n")
			fmt.Printf("  Среднее: %v\n", avg)
			fmt.Printf("  Медиана (50-й перцентиль): %v\n", p50)
			fmt.Printf("  95-й перцентиль: %v\n", p95)
			fmt.Printf("  99-й перцентиль: %v\n", p99)
			fmt.Printf("  Максимальное: %v\n", max)
			fmt.Printf("  Минимальное: %v\n", min)
		} else {
			fmt.Printf("\nНет успешных запросов для статистики latency\n")
		}
	} else {
		fmt.Printf("\nСтатистика времени ответа (все запросы):\n")
		fmt.Printf("  Среднее: %v\n", metrics.Latencies.Mean)
		fmt.Printf("  Медиана (50-й перцентиль): %v\n", metrics.Latencies.P50)
		fmt.Printf("  95-й перцентиль: %v\n", metrics.Latencies.P95)
		fmt.Printf("  99-й перцентиль: %v\n", metrics.Latencies.P99)
		fmt.Printf("  Максимальное: %v\n", metrics.Latencies.Max)
		fmt.Printf("  Минимальное: %v\n", metrics.Latencies.Min)
	}
}

// printIntervalStats выводит статистику по интервалам
func printIntervalStats(stats []IntervalStats, targetRPS int) {
	if len(stats) == 0 {
		return
	}
	fmt.Printf("%-10s %-8s %-8s %-8s %-12s %-12s %-12s %-10s %-10s\n",
		"Интервал", "Всего", "Успешно", "Ошибок", "Ср.Lat(ok)", "95%", "99%", "OK RPS", "Цель%")
	fmt.Println(strings.Repeat("-", 110))

	for i, stat := range stats {
		intervalNum := i + 1
		targetPercent := (stat.SuccessRPS / float64(targetRPS)) * 100

		// Цветовая индикация для процента достижения цели
		colorStart := ""
		colorEnd := ""
		if targetPercent < 50 {
			colorStart = "\033[31m" // Красный
			colorEnd = "\033[0m"
		} else if targetPercent < 80 {
			colorStart = "\033[33m" // Желтый
			colorEnd = "\033[0m"
		} else if targetPercent >= 95 {
			colorStart = "\033[32m" // Зеленый
			colorEnd = "\033[0m"
		}

		avgLat := stat.SuccessAvgLatency.Round(time.Millisecond)
		p95 := stat.SuccessP95Latency.Round(time.Millisecond)
		p99 := stat.SuccessP99Latency.Round(time.Millisecond)

		fmt.Printf("%02d: %s   %5d   %5d   %5d   %8v   %8v   %8v   %6.1f   %s%5.1f%%%s\n",
			intervalNum,
			stat.StartTime.Format("15:04:05"),
			stat.TotalRequests,
			stat.SuccessRequests,
			stat.ErrorRequests,
			avgLat,
			p95,
			p99,
			stat.SuccessRPS,
			colorStart,
			targetPercent,
			colorEnd)
	}
}

// printPerformanceGraphs выводит ASCII-графики
func printPerformanceGraphs(stats []IntervalStats, targetRPS int) {
	if len(stats) == 0 {
		return
	}

	// График успешного RPS
	fmt.Println("Успешный RPS по интервалам (█ - достигнутый, цель:", targetRPS, "req/s)")
	fmt.Println(strings.Repeat("-", 60))

	maxRPS := float64(0)
	for _, stat := range stats {
		if stat.SuccessRPS > maxRPS {
			maxRPS = stat.SuccessRPS
		}
	}
	if maxRPS < float64(targetRPS) {
		maxRPS = float64(targetRPS)
	}

	graphWidth := 50
	for i, stat := range stats {
		rps := stat.SuccessRPS
		barLength := int(rps / maxRPS * float64(graphWidth))
		if barLength > graphWidth {
			barLength = graphWidth
		}

		targetPercent := (rps / float64(targetRPS)) * 100
		colorStart := ""
		colorEnd := ""
		if targetPercent < 50 {
			colorStart = "\033[31m"
			colorEnd = "\033[0m"
		} else if targetPercent < 80 {
			colorStart = "\033[33m"
			colorEnd = "\033[0m"
		} else if targetPercent >= 95 {
			colorStart = "\033[32m"
			colorEnd = "\033[0m"
		}

		fmt.Printf("%02d %s %s%-*s%s %5.1f req/s\n",
			i+1,
			stat.StartTime.Format("15:04:05"),
			colorStart,
			barLength,
			strings.Repeat("█", barLength),
			colorEnd,
			rps)
	}

	// График latency успешных запросов
	fmt.Println("\nLatency (P95 успешных) по интервалам")
	fmt.Println(strings.Repeat("-", 60))

	maxLatency := time.Duration(0)
	for _, stat := range stats {
		if stat.SuccessP95Latency > maxLatency {
			maxLatency = stat.SuccessP95Latency
		}
	}
	if maxLatency == 0 {
		maxLatency = time.Second
	}

	for i, stat := range stats {
		latency := stat.SuccessP95Latency
		barLength := int(float64(latency) / float64(maxLatency) * float64(graphWidth))
		if barLength > graphWidth {
			barLength = graphWidth
		}

		colorStart := ""
		colorEnd := ""
		if latency > time.Second {
			colorStart = "\033[31m"
			colorEnd = "\033[0m"
		} else if latency > 500*time.Millisecond {
			colorStart = "\033[33m"
			colorEnd = "\033[0m"
		}

		fmt.Printf("%02d %s %s%-*s%s %v\n",
			i+1,
			stat.StartTime.Format("15:04:05"),
			colorStart,
			barLength,
			strings.Repeat("█", barLength),
			colorEnd,
			latency.Round(time.Millisecond))
	}
}

// printPercentileStats выводит перцентили (только успешные при successOnly)
func printPercentileStats(metrics *vegeta.Metrics, timeMetrics []TimeMetrics, successOnly bool) {
	if successOnly {
		latencies := extractSuccessLatencies(timeMetrics)
		if len(latencies) == 0 {
			fmt.Println("Нет успешных запросов для расчёта перцентилей")
			return
		}
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		maxLat := latencies[len(latencies)-1]
		if maxLat == 0 {
			maxLat = time.Second
		}
		percentiles := []float64{50, 75, 90, 95, 99, 99.9, 100}
		fmt.Println("Перцентили времени ответа (только успешные):")
		for _, p := range percentiles {
			idx := int(float64(len(latencies)) * p / 100)
			if idx >= len(latencies) {
				idx = len(latencies) - 1
			}
			lat := latencies[idx]
			barLen := int(float64(lat) / float64(maxLat) * 30)
			if barLen > 30 {
				barLen = 30
			}
			colorStart := ""
			colorEnd := ""
			if lat > time.Second {
				colorStart = "\033[31m"
				colorEnd = "\033[0m"
			} else if lat > 500*time.Millisecond {
				colorStart = "\033[33m"
				colorEnd = "\033[0m"
			}
			fmt.Printf("  p%-4.1f: %8v %s[%s%s]%s\n",
				p, lat.Round(time.Millisecond), colorStart,
				strings.Repeat("█", barLen),
				strings.Repeat("░", 30-barLen),
				colorEnd)
		}
	} else {
		// Стандартный вывод из vegeta
		percentiles := []float64{50, 75, 90, 95, 99, 99.9, 100}
		fmt.Println("Перцентили времени ответа (все запросы):")
		latencies := []time.Duration{
			metrics.Latencies.P50,
			metrics.Latencies.P50 + (metrics.Latencies.P95-metrics.Latencies.P50)/2,
			metrics.Latencies.P50 + (metrics.Latencies.P95-metrics.Latencies.P50)*8/10,
			metrics.Latencies.P95,
			metrics.Latencies.P99,
			metrics.Latencies.P99 + (metrics.Latencies.Max-metrics.Latencies.P99)/10,
			metrics.Latencies.Max,
		}
		maxLat := metrics.Latencies.Max
		if maxLat == 0 {
			maxLat = time.Second
		}
		for i, p := range percentiles {
			lat := latencies[i]
			barLen := int(float64(lat) / float64(maxLat) * 30)
			if barLen > 30 {
				barLen = 30
			}
			colorStart := ""
			colorEnd := ""
			if lat > time.Second {
				colorStart = "\033[31m"
				colorEnd = "\033[0m"
			} else if lat > 500*time.Millisecond {
				colorStart = "\033[33m"
				colorEnd = "\033[0m"
			}
			fmt.Printf("  p%-4.1f: %8v %s[%s%s]%s\n",
				p, lat.Round(time.Millisecond), colorStart,
				strings.Repeat("█", barLen),
				strings.Repeat("░", 30-barLen),
				colorEnd)
		}
	}
}

// printPerformanceAnalysis выводит анализ производительности
func printPerformanceAnalysis(metrics *vegeta.Metrics, duration time.Duration, targetRPS int, timeMetrics []TimeMetrics) {
	actualRPS := float64(metrics.Requests) / duration.Seconds()
	successCount := getSuccessCount(metrics)
	successRPS := float64(successCount) / duration.Seconds()
	theoreticalMax := float64(targetRPS) * duration.Seconds()

	fmt.Printf("Анализ производительности:\n")
	fmt.Printf("  Целевое количество запросов: %.0f\n", theoreticalMax)
	fmt.Printf("  Фактическое общее количество: %d\n", metrics.Requests)
	fmt.Printf("  Фактическое успешное количество: %d\n", successCount)
	fmt.Printf("  Общий RPS: %.2f req/s\n", actualRPS)
	fmt.Printf("  Успешный RPS: %.2f req/s\n", successRPS)
	fmt.Printf("  Процент успешных: %.1f%%\n", float64(successCount)/float64(metrics.Requests)*100)

}

// printErrorAnalysis выводит распределение ошибок
func printErrorAnalysis(metrics *vegeta.Metrics, timeMetrics []TimeMetrics) {
	successCount := getSuccessCount(metrics)
	if int(metrics.Requests) == successCount {
		fmt.Println("  ✅ Ошибок не обнаружено")
		return
	}

	fmt.Println("Распределение статус-кодов:")

	codes := make([]string, 0)
	for code := range metrics.StatusCodes {
		codes = append(codes, code)
	}
	sort.Strings(codes)

	for _, codeStr := range codes {
		count := metrics.StatusCodes[codeStr]
		percentage := float64(count) / float64(metrics.Requests) * 100

		var code int
		fmt.Sscanf(codeStr, "%d", &code)

		category := ""
		colorStart := ""
		colorEnd := "\033[0m"
		switch {
		case code >= 200 && code < 300:
			category = "✅ Успех"
			colorStart = "\033[32m"
		case code >= 300 && code < 400:
			category = "🔄 Перенаправление"
			colorStart = "\033[36m"
		case code >= 400 && code < 500:
			category = "❌ Ошибка клиента"
			colorStart = "\033[33m"
		case code >= 500:
			category = "💥 Ошибка сервера"
			colorStart = "\033[31m"
		default:
			category = "📊 Другое"
		}

		fmt.Printf("  %s%s %s: %d (%.2f%%)%s\n",
			colorStart, category, codeStr, count, percentage, colorEnd)
	}
}

// saveToFile сохраняет отчёт в файл
func saveToFile(metrics *vegeta.Metrics, duration time.Duration, intervalStats []IntervalStats, timeMetrics []TimeMetrics, targetRPS int, successOnly bool) {
	filename := fmt.Sprintf("load_test_report_%s.txt", time.Now().Format("2006-01-02_15-04-05"))

	file, err := os.Create(filename)
	if err != nil {
		log.Printf("Ошибка при создании файла: %v", err)
		return
	}
	defer file.Close()

	fmt.Fprintf(file, "ДЕТАЛЬНЫЙ ОТЧЕТ НАГРУЗОЧНОГО ТЕСТИРОВАНИЯ\n")
	fmt.Fprintf(file, "============================================\n")
	fmt.Fprintf(file, "Дата и время: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(file, "URL: %s\n", flag.Lookup("url").Value.String())
	fmt.Fprintf(file, "Целевой RPS: %d\n", targetRPS)
	fmt.Fprintf(file, "Длительность: %v\n", duration)
	fmt.Fprintf(file, "Учитывать только успешные: %v\n", successOnly)
	fmt.Fprintf(file, "============================================\n\n")

	actualRPS := float64(metrics.Requests) / duration.Seconds()
	successCount := getSuccessCount(metrics)
	successRPS := float64(successCount) / duration.Seconds()
	theoreticalMax := float64(targetRPS) * duration.Seconds()

	fmt.Fprintf(file, "ОСНОВНАЯ СТАТИСТИКА\n")
	fmt.Fprintf(file, "-------------------\n")
	fmt.Fprintf(file, "Всего запросов: %d\n", metrics.Requests)
	fmt.Fprintf(file, "Успешных запросов: %d\n", successCount)
	fmt.Fprintf(file, "Успешность: %.2f%%\n", metrics.Success*100)
	fmt.Fprintf(file, "Общий RPS: %.2f req/s\n", actualRPS)
	fmt.Fprintf(file, "Успешный RPS: %.2f req/s\n", successRPS)
	fmt.Fprintf(file, "Достигнуто цели (успешный RPS): %.1f%%\n", (successRPS/float64(targetRPS))*100)
	fmt.Fprintf(file, "Целевое кол-во: %.0f, Фактическое: %d, Разница: %d\n\n",
		theoreticalMax, metrics.Requests, int(theoreticalMax)-int(metrics.Requests))

	if successOnly {
		successLatencies := extractSuccessLatencies(timeMetrics)
		if len(successLatencies) > 0 {
			sort.Slice(successLatencies, func(i, j int) bool { return successLatencies[i] < successLatencies[j] })
			avg := calculateAverage(successLatencies)
			p50 := successLatencies[len(successLatencies)*50/100]
			p95 := successLatencies[len(successLatencies)*95/100]
			p99 := successLatencies[len(successLatencies)*99/100]
			max := successLatencies[len(successLatencies)-1]
			min := successLatencies[0]

			fmt.Fprintf(file, "СТАТИСТИКА ВРЕМЕНИ ОТВЕТА (только успешные)\n")
			fmt.Fprintf(file, "------------------------------------------\n")
			fmt.Fprintf(file, "Среднее: %v\n", avg)
			fmt.Fprintf(file, "Медиана (50%%): %v\n", p50)
			fmt.Fprintf(file, "95%%: %v\n", p95)
			fmt.Fprintf(file, "99%%: %v\n", p99)
			fmt.Fprintf(file, "Максимальное: %v\n", max)
			fmt.Fprintf(file, "Минимальное: %v\n\n", min)
		}
	} else {
		fmt.Fprintf(file, "СТАТИСТИКА ВРЕМЕНИ ОТВЕТА (все запросы)\n")
		fmt.Fprintf(file, "--------------------------------------\n")
		fmt.Fprintf(file, "Среднее: %v\n", metrics.Latencies.Mean)
		fmt.Fprintf(file, "Медиана (50%%): %v\n", metrics.Latencies.P50)
		fmt.Fprintf(file, "95%%: %v\n", metrics.Latencies.P95)
		fmt.Fprintf(file, "99%%: %v\n", metrics.Latencies.P99)
		fmt.Fprintf(file, "Максимальное: %v\n", metrics.Latencies.Max)
		fmt.Fprintf(file, "Минимальное: %v\n\n", metrics.Latencies.Min)
	}

	if len(intervalStats) > 0 {
		fmt.Fprintf(file, "СТАТИСТИКА ПО ИНТЕРВАЛАМ (по времени отправки)\n")
		fmt.Fprintf(file, "--------------------------------------------\n")
		for i, stat := range intervalStats {
			fmt.Fprintf(file, "Интервал %d (%s - %s):\n", i+1,
				stat.StartTime.Format("15:04:05"), stat.EndTime.Format("15:04:05"))
			fmt.Fprintf(file, "  Всего запросов: %d\n", stat.TotalRequests)
			fmt.Fprintf(file, "  Успешных: %d\n", stat.SuccessRequests)
			fmt.Fprintf(file, "  Ошибок: %d\n", stat.ErrorRequests)
			fmt.Fprintf(file, "  Успешный RPS: %.2f\n", stat.SuccessRPS)
			fmt.Fprintf(file, "  Достигнуто цели: %.1f%%\n", (stat.SuccessRPS/float64(targetRPS))*100)
			if stat.SuccessRequests > 0 {
				fmt.Fprintf(file, "  Средний latency успешных: %v\n", stat.SuccessAvgLatency)
				fmt.Fprintf(file, "  95%% latency успешных: %v\n", stat.SuccessP95Latency)
				fmt.Fprintf(file, "  99%% latency успешных: %v\n", stat.SuccessP99Latency)
				fmt.Fprintf(file, "  Max latency успешных: %v\n", stat.SuccessMaxLatency)
				fmt.Fprintf(file, "  Min latency успешных: %v\n", stat.SuccessMinLatency)
			}
			fmt.Fprintf(file, "\n")
		}
	}

	fmt.Fprintf(file, "РАСПРЕДЕЛЕНИЕ СТАТУС-КОДОВ\n")
	fmt.Fprintf(file, "--------------------------\n")
	for code, count := range metrics.StatusCodes {
		fmt.Fprintf(file, "  %s: %d\n", code, count)
	}

	fmt.Printf("\nДетальный отчет сохранен в файл: %s\n", filename)
}
