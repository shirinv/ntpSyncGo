package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"time"
	"unsafe"

	"github.com/andlabs/ui"
	_ "github.com/andlabs/ui/winmanifest"
	"github.com/beevik/ntp"
	"golang.org/x/sys/windows"
)

var (
	modkernel32       = windows.NewLazySystemDLL("kernel32.dll")
	procSetSystemTime = modkernel32.NewProc("SetSystemTime")

	predefinedServers = []string{
		"pool.ntp.org",
		"time.google.com",
		"time.windows.com",
		"time.apple.com",
		"europe.pool.ntp.org",
		"north-america.pool.ntp.org",
		"asia.pool.ntp.org",
	}

	selectedServer string

	updateTicker *time.Ticker
	updateQuit   chan struct{}

	autoSyncTicker *time.Ticker
	autoSyncQuit   chan struct{}
)

func main() {
	err := ui.Main(func() {
		window := ui.NewWindow("NTP Sync", 500, 600, false)
		window.OnClosing(func(*ui.Window) bool {
			if updateTicker != nil {
				updateTicker.Stop()
				close(updateQuit)
			}
			if autoSyncTicker != nil {
				autoSyncTicker.Stop()
				close(autoSyncQuit)
			}
			ui.Quit()
			return true
		})

		// Создаем элементы интерфейса
		timeLabel := ui.NewLabel("Получение NTP времени...")
		localTimeLabel := ui.NewLabel("Системное время: ...")
		logText := ui.NewMultilineEntry()
		logText.SetReadOnly(true)

		// Функция для добавления записей в лог
		addLog := func(message string) {
			logMessage := fmt.Sprintf("%s: %s\n", time.Now().Format("2006-01-02 15:04:05"), message)
			logText.Append(logMessage)
		}

		// Элементы для выбора сервера
		serverSelect := ui.NewCombobox()
		serverSelect.Append("Выберите NTP-сервер")
		for _, server := range predefinedServers {
			serverSelect.Append(server)
		}
		serverSelect.SetSelected(0) // По умолчанию выбран первый элемент

		customServerEntry := ui.NewEntry()
		customServerEntry.SetText("")

		syncButton := ui.NewButton("Синхронизировать системное время")
		syncIntervalEntry := ui.NewEntry()
		syncIntervalEntry.SetText("")
		// Изменение текста подсказки
		syncIntervalEntry.SetText("Введите интервал синхронизации (в секундах)")

		startSyncButton := ui.NewButton("Запустить автоматическую синхронизацию")

		// Обработчики событий
		serverSelect.OnSelected(func(combobox *ui.Combobox) {
			index := serverSelect.Selected()
			if index > 0 {
				selectedServer = predefinedServers[index-1]
				startUpdatingNTPTime(selectedServer, timeLabel, addLog)
			} else {
				selectedServer = ""
			}
		})

		customServerEntry.OnChanged(func(entry *ui.Entry) {
			text := entry.Text()
			if text != "" {
				selectedServer = text
				startUpdatingNTPTime(selectedServer, timeLabel, addLog)
			} else {
				selectedServer = ""
			}
		})

		syncButton.OnClicked(func(*ui.Button) {
			var server string
			if customServerEntry.Text() != "" {
				server = customServerEntry.Text()
			} else if selectedServer != "" {
				server = selectedServer
			} else {
				ui.MsgBoxError(window, "Ошибка", "Пожалуйста, выберите или введите NTP-сервер")
				return
			}

			ntpTime, err := getNTPTime(server)
			if err != nil {
				addLog(fmt.Sprintf("Ошибка получения времени с сервера %s: %v", server, err))

				return
			}
			timeLabel.SetText("Синхронизированное время: " + ntpTime.Format("15:04:05 MST 2006-01-02"))
			addLog(fmt.Sprintf("Синхронизированное время с сервера %s: %s", server, ntpTime.Format("15:04:05 MST 2006-01-02")))

			// Устанавливаем системное время
			err = setSystemTime(ntpTime)
			if err != nil {
				addLog("Ошибка")
				return
			}
			ui.MsgBox(window, "Успешно", "Системное время синхронизировано")
		})

		startSyncButton.OnClicked(func(*ui.Button) {
			intervalText := syncIntervalEntry.Text()
			interval, err := strconv.Atoi(intervalText)
			if err != nil || interval <= 0 {
				addLog("Ошибка, пожалуйста, введите корректный положительный интервал в секундах")

				return
			}
			startAutoSync(interval, window, addLog)
		})

		// Создаем компоновку элементов
		box := ui.NewVerticalBox()
		box.SetPadded(true)
		box.Append(ui.NewLabel("NTP Синхронизация времени"), false)
		box.Append(serverSelect, false)
		box.Append(customServerEntry, false)
		box.Append(syncButton, false)
		box.Append(ui.NewLabel("Настройка автоматической синхронизации"), false)
		box.Append(syncIntervalEntry, false)
		box.Append(startSyncButton, false)
		box.Append(timeLabel, false)
		box.Append(localTimeLabel, false)
		box.Append(ui.NewLabel("Логи синхронизации"), false)
		box.Append(logText, true)

		window.SetChild(box)
		window.Show()

		// Канал для передачи обновлений метки системного времени
		updateTimeChannel := make(chan string)

		// Запускаем обновление системного времени
		go func() {
			for {
				time.Sleep(1 * time.Second)
				currentTime := time.Now()
				labelText := "Системное время: " + currentTime.Format("15:04:05 MST 2006-01-02")
				updateTimeChannel <- labelText
			}
		}()

		// Обновляем интерфейс в главном потоке
		go func() {
			for labelText := range updateTimeChannel {
				ui.QueueMain(func() {
					localTimeLabel.SetText(labelText)
				})
			}
		}()
	})
	if err != nil {
		panic(err)
	}
}

func startAutoSync(interval int, window *ui.Window, addLog func(string)) {
	// Если тикер уже запущен, остановим его
	if autoSyncTicker != nil {
		autoSyncTicker.Stop()
		close(autoSyncQuit)
	}

	// Создаем новый тикер с заданным интервалом (в секундах)
	autoSyncTicker = time.NewTicker(time.Duration(interval) * time.Second)
	autoSyncQuit = make(chan struct{})

	go func() {
		for {
			select {
			case <-autoSyncQuit:
				return
			case <-autoSyncTicker.C:
				if selectedServer == "" {
					logMessage := "NTP-сервер не выбран для синхронизации"
					addLog(logMessage)

					continue
				}
				ntpTime, err := getNTPTime(selectedServer)
				if err != nil {
					logMessage := fmt.Sprintf("Ошибка получения времени с сервера %s: %v", selectedServer, err)
					addLog(logMessage)

					continue
				}
				// Устанавливаем системное время
				err = setSystemTime(ntpTime)
				if err != nil {
					logMessage := fmt.Sprintf("Ошибка установки системного времени: %v", err)
					addLog(logMessage)

					continue
				}
				addLog("Системное время успешно синхронизировано")

			}
		}
	}()
}

// Функция для периодического обновления NTP времени
func startUpdatingNTPTime(server string, label *ui.Label, addLog func(string)) {
	// Если тикер уже запущен, останавливаем его
	if updateTicker != nil {
		updateTicker.Stop()
		close(updateQuit)
	}

	// Создаем новый тикер для обновления NTP времени каждую 1 секунду
	updateTicker = time.NewTicker(1 * time.Second)
	updateQuit = make(chan struct{})

	go func() {
		for {
			select {
			case <-updateQuit:
				return
			case <-updateTicker.C:
				ntpTime, err := getNTPTime(server)
				if err != nil {
					// Показываем уведомление об ошибке
					addLog("Ошибка получения времени")
					continue
				}
				// Обновляем метку в главном потоке
				labelText := "Текущее NTP время: " + ntpTime.Format("15:04:05 MST 2006-01-02")
				ui.QueueMain(func() {
					label.SetText(labelText)
				})
			}
		}
	}()
}

// Функция для получения NTP времени
func getNTPTime(server string) (time.Time, error) {
	return ntp.Time(server)
}

// Установка системного времени
func setSystemTime(t time.Time) error {
	if runtime.GOOS == "windows" {
		t = t.UTC()
		st := &windows.Systemtime{
			Year:         uint16(t.Year()),
			Month:        uint16(t.Month()),
			DayOfWeek:    uint16(t.Weekday()),
			Day:          uint16(t.Day()),
			Hour:         uint16(t.Hour()),
			Minute:       uint16(t.Minute()),
			Second:       uint16(t.Second()),
			Milliseconds: uint16(t.Nanosecond() / 1e6),
		}

		ret, _, err := procSetSystemTime.Call(uintptr(unsafe.Pointer(st)))
		if ret == 0 {
			return fmt.Errorf("не удалось установить системное время: %v", err)
		}
		return nil
	} else {
		cmd := exec.Command("sudo", "date", "-s", t.Format("2006-01-02 15:04:05"))
		return cmd.Run()
	}
}
