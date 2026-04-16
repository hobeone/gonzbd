// This file is generated — do not hand-edit.
// Regenerate via scripts/gen_glitter_js.py.
//
// Source of truth: upstream sabnzbd/interfaces/Glitter/templates/static/javascripts/
// glitter.js (a Cheetah template with raw-include directives). Python SABnzbd
// expands those includes at render time; the Go port pre-assembles equivalently
// so the browser receives plain JavaScript.

/******

        Glitter V1
        By Safihre (2015) - safihre@sabnzbd.org

        Code extended from Shiny-template
        Code examples used from Knockstrap-template

        The setup is hierarchical, 1 main ViewModel that contains:
        - ViewModel
            - QueueListModel
                - paginationModel
                - QueueModel (item 1)
                - QueueModel (item 2)
                - ...
                - QueueModel (item n+1)
            - HistoryListModel
                - paginationModel
                - HistoryModel (item 1)
                - HistoryModel (item 2)
                - ...
                - HistoryModel (item n+1)
            - Fileslisting
                - FileslistingModel (file 1)
                - FileslistingModel (file 2)
                - ...
                - FileslistingModel (file n+1)

        ViewModel also contains all the code executed on document ready and
        functions responsible for the status information, adding NZB, etc.
        The QueueModel/HistoryModel's get added to the list-models when
        jobs are added or on switching of pages (using paginationModel).
        Once added only the properties that changed during a refresh
        get updated. In the history all the detailed information is only
        updated when created and when the user clicks on a detail.
        The Fileslisting is only populated and updated when it is opened
        for one of the QueueModel's.

******/

/**
    Base variables and functions
**/
var isMobile = (/android|webos|iphone|ipad|ipod|blackberry|iemobile|opera mini/i.test(navigator.userAgent.toLowerCase()));

// To avoid problems when localStorage is disabled
var hasLocalStorage = true;
function localStorageSetItem(varToSet, valueToSet) { try { return localStorage.setItem(varToSet, valueToSet); } catch(e) { hasLocalStorage = false; } }
function localStorageGetItem(varToGet) { try { return localStorage.getItem(varToGet); } catch(e) {  hasLocalStorage = false; } }

// For mobile we disable zoom while a modal is being opened
// so it will not zoom unnecessarily on the modal
if(isMobile) {
    $('.modal').on('show.bs.modal', function() {
        $('meta[name="viewport"]').attr('content', 'width=device-width, initial-scale=1, maximum-scale=1, user-scalable=no');
    });

    // Restore on modal-close. Need timeout, otherwise it doesn't work
    $('.modal').on('hidden.bs.modal', function() {
        setTimeout(function() {
            $('meta[name="viewport"]').attr('content', 'width=device-width, initial-scale=1');
        },500);
    });
}

// Basic API-call
function callAPI(data, timeout = 10000) {
    // Fill basis var's
    data.output = "json";
    data.apikey = apiKey;
    var ajaxQuery = $.ajax({
        url: "./api",
        type: "GET",
        cache: false,
        data: data,
        timeout: timeout
    });

    return $.when(ajaxQuery);
}

/***
    GENERAL FUNCTIONS
***/
// Function to fix percentages
function fixPercentages(intPercent) {
    // Skip NaN's
    if(isNaN(intPercent))
        intPercent = 0;
    return Math.floor(intPercent || 0) + '%';
}

// Convert HTML tags to regular text
function convertHTMLtoText(htmltxt) {
    return $('<div>').text(htmltxt).html().replace(/&lt;br\/&gt;/g, '<br/>')
}

// Function to re-write 0:09:21=>9:21, 0:10:10=>10:10, 0:00:30=>0:30
function rewriteTime(timeString) {
    // Remove "0:0" from start
    if(timeString.substring(0,3) === '0:0') {
        timeString = timeString.substring(3)
    }
    // Remove "0:" from start
    else if(timeString.substring(0,2) === '0:') {
        timeString = timeString.substring(2)
    }
    return timeString
}

// How to display the date-time?
function displayDateTime(inDate, outFormat, inFormat) {
    // What input?
    if(inDate === '') {
        var theMoment = moment()
    } else {
        var theMoment = moment.utc(inDate, inFormat)
    }
    // Special format or regular format?
    if(outFormat === 'fromNow') {
        return theMoment.fromNow()
    } else {
        return theMoment.local().format(outFormat)
    }
}

// Keep dropdowns open
function keepOpen(thisItem) {
    // Make sure we clicked the a and not the glyphicon/caret!
    if(!$(thisItem).is('a') && !$(thisItem).is('button')) {
        // Do it again on the parent
        keepOpen(thisItem.parentElement)
        return;
    }

    // Onlick so it works for the dynamic items!
    $(thisItem).siblings('.dropdown-menu').children().click(function(e) {
        // Not for links
        if(!$(e.target).is('a')) {
            e.stopPropagation();
        }
    });
    // Add possible tooltips and make sure they get removed
    if(!isMobile)  {
        $(thisItem).siblings('.dropdown-menu').children('[data-tooltip="true"]').tooltip({ trigger: 'hover', container: 'body' })
        $(thisItem).parent().on('hide.bs.dropdown', function() {
            $(thisItem).siblings('.dropdown-menu').children('[data-tooltip="true"]').tooltip('hide')
        })
    }
}

// Show history details
function showDetails(thisItem) {
    // Unfortunatly the .dropdown('toggle') doesn't work in this setup, so work-a-round

    // Open the details of this, or close it?
    if($(thisItem).parent().find('.delete>.dropdown').hasClass('open')) {
        // One click = close
        $(thisItem).parent().find('.delete>.dropdown>a').click()
    } else {
        // Needs timeout, otherwise it thinks its the 'close' click for some reason
        setTimeout(function() {
            $(thisItem).parent().find('.delete>.dropdown>a').click()
        },1)
    }
}

// Check all functionality
function checkAllFiles(objCheck, onlyCheck) {
    // Get which ones we care about
    var allChecks = $($(objCheck).data('checkrange')).filter(':not(:disabled):visible');

    // We need to re-evaltuate the state of this check-all
    // Otherwise the 'inderterminate' will be overwritten by the click event!
    setCheckAllState('#'+objCheck.id, $(objCheck).data('checkrange'))

    // Now we can check what happend
    if(objCheck.indeterminate) {
        // Uncheck if we don't need trigger
        if(onlyCheck) {
            allChecks.filter(":checked").prop('checked', false)
        } else  {
            allChecks.filter(":checked").trigger("click")
        }
    } else {
        // Toggle their state by a click
        allChecks.trigger("click")
    }
}

// To update the check-all button nicely
function setCheckAllState(checkSelector, rangeSelector) {
    // See how many are checked
    var allChecks = $(rangeSelector).filter(':not(:disabled):visible')
    var nrChecks = allChecks.filter(":checked");
    if(nrChecks.length === 0) {
        $(checkSelector).prop({'checked': false, 'indeterminate': false})
    } else if(nrChecks.length === allChecks.length) {
        $(checkSelector).prop({'checked': true, 'indeterminate': false})
    } else {
        $(checkSelector).prop({'checked': false, 'indeterminate': true})
    }
}

// Shift-range functionality for checkboxes
function checkShiftRange(strCheckboxes) {
    // Get them all
    var arrAllChecks = $(strCheckboxes);
    // Get index of the first and last
    var startCheck = arrAllChecks.index($(strCheckboxes + ':checked:first'));
    var endCheck = arrAllChecks.index($(strCheckboxes + ':checked:last'));
    // Everything in between click it to trigger addMultiEdit
    arrAllChecks.slice(startCheck, endCheck).filter(':not(:checked)').trigger('click')
}

// Hide completed files in files-modal
function hideCompletedFiles() {
    if($('#filelist-showcompleted').hasClass('hover-button')) {
        // Hide all
        $('.item-files-table tr.files-done').hide();
        $('#filelist-showcompleted').removeClass('hover-button')
        // Set storage
        localStorageSetItem('showCompletedFiles', 'No')
    } else {
        // show all
        $('.item-files-table tr.files-done').show();
        $('#filelist-showcompleted').addClass('hover-button')
        // Set storage
        localStorageSetItem('showCompletedFiles', 'Yes')
    }
}

// Show status modal and switch to orphaned jobs tab
function showOrphans() {
    $('a[href="#modal-options"]').click().parent().click();
    $('a[href="#options-orphans"]').click()
}

// Show notification
function showNotification(notiName, notiTimeout, fileCounter) {
    // Set uploadcounter if there is one
    $('.main-notification-box .main-notification-box-file-count').text(fileCounter)

    // Hide others, show the new one
    $('.main-notification-box>div').hide()
    $(notiName).css('display', 'inline')
    // Only fade in when hidden
    $('.main-notification-box:hidden').fadeIn()

    // Remove after timeout
    if(notiTimeout) {
        setTimeout(function() {
            hideNotification();
        }, notiTimeout)
    }
}

// Hide notification
function hideNotification() {
    // Hide the box with effect
    $('.main-notification-box').fadeOut()
}

/**
    GLITTER CODE
**/
$(function() {
    'use strict';

/**
    Define main view model
**/
function ViewModel() {
    // Initialize models
    var self = this;
    self.queue = new QueueListModel(this);
    self.history = new HistoryListModel(this);
    self.filelist = new Fileslisting(this);

    // Set status varibales
    self.isRestarting = ko.observable(false);
    self.useGlobalOptions = ko.observable(true).extend({ persist: 'useGlobalOptions' });
    self.refreshRate = ko.observable(1).extend({ persist: 'pageRefreshRate' });
    self.dateFormat = ko.observable('fromNow').extend({ persist: 'pageDateFormat' });
    self.displayTabbed = ko.observable(false).extend({ persist: 'displayTabbed' });
    self.displayCompact = ko.observable(false).extend({ persist: 'displayCompact' });
    self.displayFullWidth = ko.observable(false).extend({ persist: 'displayFullWidth' });
    self.confirmDeleteQueue = ko.observable(true).extend({ persist: 'confirmDeleteQueue' });
    self.confirmDeleteHistory = ko.observable(true).extend({ persist: 'confirmDeleteHistory' });
    self.keyboardShortcuts = ko.observable(true).extend({ persist: 'keyboardShortcuts' });
    self.extraQueueColumns = ko.observableArray([]).extend({ persist: 'extraColumns' });
    self.extraHistoryColumns = ko.observableArray([]).extend({ persist: 'extraHistoryColumns' });
    self.showActiveConnections = ko.observable(false).extend({ persist: 'showActiveConnections' });
    self.speedMetrics = { '': "B/s", K: "KB/s", M: "MB/s", G: "GB/s" };

    // Set information varibales
    self.title = ko.observable();
    self.speed = ko.observable(0);
    self.speedMetric = ko.observable();
    self.bandwithLimit = ko.observable(false);
    self.pauseCustom = ko.observable('').extend({ rateLimit: { timeout: 200, method: "notifyWhenChangesStop" } });
    self.speedLimit = ko.observable(100).extend({ rateLimit: { timeout: 200, method: "notifyWhenChangesStop" } });
    self.speedLimitInt = ko.observable(false); // We need the 'internal' counter so we don't trigger the API all the time
    self.downloadsPaused = ko.observable(false);
    self.timeLeft = ko.observable("0:00");
    self.diskSpaceLeft1 = ko.observable();
    self.diskSpaceLeft2 = ko.observable();
    self.queueDataLeft = ko.observable();
    self.diskSpaceExceeded1 = ko.observable(false);
    self.diskSpaceExceeded2 = ko.observable(false);
    self.quotaLimit = ko.observable();
    self.quotaLimitLeft = ko.observable();
    self.systemLoad = ko.observable();
    self.cacheSize = ko.observable();
    self.cacheArticles = ko.observable();
    self.loglevel = ko.observable();
    self.nrWarnings = ko.observable(0);
    self.allWarnings = ko.observableArray([]);
    self.allMessages = ko.observableArray([]);
    self.finishaction = ko.observable();
    self.speedHistory = [];

    // Statusinfo container
    self.hasStatusInfo = ko.observable(false);
    self.hasPerformanceInfo = ko.observable(false);
    self.statusInfo = {};
    self.statusInfo.folders = ko.observableArray([]);
    self.statusInfo.servers = ko.observableArray([]);
    self.statusInfo.active_socks5_proxy = ko.observable();
    self.statusInfo.localipv4 = ko.observable();
    self.statusInfo.publicipv4 = ko.observable();
    self.statusInfo.ipv6 = ko.observable();
    self.statusInfo.dnslookup = ko.observable();
    self.statusInfo.delayed_assembler = ko.observable();
    self.statusInfo.loadavg = ko.observable();
    self.statusInfo.pystone = ko.observable();
    self.statusInfo.downloaddir = ko.observable();
    self.statusInfo.downloaddirspeed = ko.observable();
    self.statusInfo.completedir = ko.observable();
    self.statusInfo.completedirspeed = ko.observable();
    self.statusInfo.internetbandwidth = ko.observable();

    /***
        Dynamic functions
    ***/

    // Make the speedlimit tekst
    self.speedLimitText = ko.pureComputed(function() {
        // Set?
        if (!self.bandwithLimit()) return;

        // The text
        var bandwithLimitText = self.bandwithLimit().replace(/[^a-zA-Z]+/g, '');

        // Only the number
        var speedLimitNumberFull = (parseFloat(self.bandwithLimit()) * (self.speedLimit() / 100));

        // Trick to only get decimal-point when needed
        var speedLimitNumber = Math.round(speedLimitNumberFull * 10) / 10;

        // Fix it for lower than 1MB/s
        if (bandwithLimitText === 'M' && speedLimitNumber < 1) {
            bandwithLimitText = 'K';
            speedLimitNumber = Math.round(speedLimitNumberFull * 1024);
        }

        // Show text
        return self.speedLimit() + '% (' + speedLimitNumber + ' ' + self.speedMetrics[bandwithLimitText] + ')';
    });

    // Dynamic speed text function
    self.speedText = ko.pureComputed(function() {
        return self.speed() + ' ' + (self.speedMetrics[self.speedMetric()] ? self.speedMetrics[self.speedMetric()] : "B/s");
    });

    // Dynamic icon
    self.SABIcon = ko.pureComputed(function() {
        if (self.downloadsPaused()) {
            return './staticcfg/ico/faviconpaused.ico?v=1.1.0';
        } else {
            return './staticcfg/ico/favicon.ico?v=1.1.0';
        }
    })

    // Dynamic queue length check
    self.hasQueue = ko.pureComputed(function() {
        return (self.queue.queueItems().length > 0 || self.queue.searchTerm() || self.queue.isLoading())
    })

    // Dynamic history length check
    self.hasHistory = ko.pureComputed(function() {
        return (self.history.historyItems().length > 0 || self.history.searchTerm() || self.history.isLoading())
    })

    self.hasWarnings = ko.pureComputed(function() {
        return (self.allWarnings().length > 0)
    })

    // Check for any warnings/messages
    self.hasMessages = ko.pureComputed(function() {
        return parseInt(self.nrWarnings()) + self.allMessages().length;
    })

    // Update main queue
    self.updateQueue = function(response) {
        // Block in case off dragging
        if (!self.queue.shouldUpdate()) return;

        // Make sure we are displaying the interface
        if (self.isRestarting() >= 1) {
            // Decrease the counter by 1
            // In case of restart (which takes time to fire) we count down
            // In case of re-connect after failure it counts from 1 so emmediate continuation
            self.isRestarting(self.isRestarting() - 1);
            return;
        }

        /***
            Possible login failure?
        ***/
        if (response.hasOwnProperty('error') && response.error === 'Missing authentication') {
            // Restart
            document.location = document.location;
        }

        /***
            Basic information
        ***/
        // Queue left
        self.queueDataLeft(response.queue.mbleft > 0 ? response.queue.sizeleft : '')

        // Paused?
        self.downloadsPaused(response.queue.paused);

        // Finish action. Replace null with empty
        self.finishaction(response.queue.finishaction ? response.queue.finishaction : '');

        // Disk sizes
        self.diskSpaceLeft1(response.queue.diskspace1_norm)

        // Same sizes? Then it's all 1 disk!
        if (response.queue.diskspace1 !== response.queue.diskspace2) {
            self.diskSpaceLeft2(response.queue.diskspace2_norm)
        } else {
            self.diskSpaceLeft2('')
        }

        // Did we exceed the space?
        self.diskSpaceExceeded1(parseInt(response.queue.mbleft) / 1024 > parseFloat(response.queue.diskspace1))
        self.diskSpaceExceeded2(parseInt(response.queue.mbleft) / 1024 > parseFloat(response.queue.diskspace2))

        // Quota
        self.quotaLimit(response.queue.quota)
        self.quotaLimitLeft(response.queue.left_quota)

        // Cache
        self.cacheSize(response.queue.cache_size)
        self.cacheArticles(response.queue.cache_art)

        // Warnings (new warnings will trigger an update of allMessages)
        self.nrWarnings(response.queue.have_warnings)

        /***
            Spark line
        ***/
        // Break the speed if empty queue
        if (response.queue.sizeleft === '0 B') {
            response.queue.kbpersec = 0;
            response.queue.speed = '0';
        }

        // Re-format the speed
        var speedSplit = response.queue.speed.split(/\s/);
        self.speed(parseFloat(speedSplit[0]));
        self.speedMetric(speedSplit[1]);

        // Update sparkline data
        if (self.speedHistory.length >= 275) {
            // Remove first one
            self.speedHistory.shift();
        }
        // Add
        self.speedHistory.push(parseInt(response.queue.kbpersec));

        // Is sparkline visible? Not on small mobile devices..
        if ($('.sparkline-container').css('display') !== 'none') {
            // Make sparkline
            if (self.speedHistory.length === 1) {
                // We only use speedhistory from SAB if we use global settings
                // Otherwise SAB doesn't know the refresh rate
                if (!self.useGlobalOptions()) {
                    sabSpeedHistory = [];
                } else {
                    // Update internally
                    self.speedHistory = sabSpeedHistory;
                }

                // Create
                $('.sparkline').peity("line", {
                    width: 275,
                    height: 32,
                    fill: '#9DDB72',
                    stroke: '#AAFFAA',
                    values: sabSpeedHistory
                })

                // Add option to open the server details tab
                $('.sparkline-container').click(function() {
                    $('a[href="#modal-options"]').trigger('click')
                    $('a[href="#options_connections"]').trigger('click')
                })

            } else {
                // Update
                $('.sparkline').text(self.speedHistory.join(",")).change()
            }
        }

        /***
            Speedlimit
        ***/
        // Nothing or 0 means 100%
        if(response.queue.speedlimit === '' || response.queue.speedlimit === '0') {
            self.speedLimitInt(100)
        } else {
            self.speedLimitInt(parseInt(response.queue.speedlimit));
        }

        // Only update from external source when user isn't doing input
        if (!$('.speedlimit-dropdown .btn-group .btn-group').is('.open')) {
            self.speedLimit(self.speedLimitInt())
        }

        /***
            Download timing and pausing
        ***/
        var timeString = response.queue.timeleft;
        if (timeString === '') {
            timeString = '0:00';
        } else {
            timeString = rewriteTime(response.queue.timeleft)
        }

        // Paused main queue
        if (self.downloadsPaused()) {
            if (response.queue.pause_int === '0') {
                timeString = glitterTranslate.paused;
            } else {
                var pauseSplit = response.queue.pause_int.split(/:/);
                var seconds = parseInt(pauseSplit[0]) * 60 + parseInt(pauseSplit[1]);
                var hours = Math.floor(seconds / 3600);
                var minutes = Math.floor((seconds -= hours * 3600) / 60);
                seconds -= minutes * 60;

                // Add leading zeros
                if (minutes < 10) minutes = '0' + minutes;
                if (seconds < 10) seconds = '0' + seconds;

                // Final formating
                timeString = glitterTranslate.paused + ' (' + rewriteTime(hours + ":" + minutes + ":" + seconds) + ')';
            }

            // Add info about amount of download (if actually downloading)
            if (response.queue.noofslots > 0 && parseInt(self.queueDataLeft()) > 0) {
                self.title(timeString + ' - ' + self.queueDataLeft() + ' ' + glitterTranslate.left + ' - SABnzbd')
            } else {
                // Set title with pause information
                self.title(timeString + ' - SABnzbd')
            }
        } else if (response.queue.noofslots > 0 && parseInt(self.queueDataLeft()) > 0) {
            // Set title only if we are actually downloading something..
            self.title(self.speedText() + ' - ' + self.queueDataLeft() + ' ' + glitterTranslate.left + ' - SABnzbd')
        } else {
            // Empty title
            self.title('SABnzbd')
        }

        // Save for timing box
        self.timeLeft(timeString);

        // Update queue rows
        self.queue.updateFromData(response.queue);
    }

    // Update history items
    self.updateHistory = function(response) {
        if (!response) return;
        self.history.updateFromData(response.history);
    }

    // Set new update timer
    self.setNextUpdate = function() {
        self.interval = setTimeout(self.refresh, parseInt(self.refreshRate()) * 1000);
    }

    // Refresh function
    self.refresh = function(forceFullHistory) {
        // Clear previous timeout to prevent double-calls
        clearTimeout(self.interval);

        // Do requests for full information
        // Catch the fail to display message
        var api_call = {
            mode: "queue",
            start: self.queue.pagination.currentStart(),
            limit: parseInt(self.queue.paginationLimit())
        }
        if (self.queue.searchTerm()) {
            parseSearchQuery(api_call, self.queue.searchTerm(), ["cat", "category", "priority", "status"])
        }
        var queueApi = callAPI(api_call)
            .done(self.updateQueue)
            .fail(function(response) {
                // Catch the failure of authorization error
                if (response.status === 401) {
                    // Stop refresh and reload
                    clearInterval(self.interval)
                    location.reload();
                }
                // Show screen
                self.isRestarting(1)
            }).always(self.setNextUpdate);

        // Force full history update?
        if (forceFullHistory) {
            self.history.lastUpdate = 0
        }

        // Build history request and parse search
        var history_call = {
            mode: "history",
            failed_only: self.history.showFailed() * 1,
            start: self.history.pagination.currentStart(),
            limit: parseInt(self.history.paginationLimit()),
            archive: self.history.showArchive() * 1,
            last_history_update: self.history.lastUpdate
        }
        if (self.history.searchTerm()) {
            parseSearchQuery(history_call, self.history.searchTerm(), ["cat", "category", "status"])
        }

        // History
        callAPI(history_call).done(self.updateHistory);

        // We are now done with any loading
        // But we wait a few ms so Knockout has time to update
        setTimeout(function() {
            self.queue.isLoading(false);
            self.history.isLoading(false);
        }, 100)

        // Return for .then() functionality
        return queueApi;
    };

    function parseSearchQuery(api_request, search, keywords) {
        var parsed_query = search_query_parse(search, { keywords: keywords })
        api_request["search"] = parsed_query.text
        for (const keyword of keywords) {
            if (Array.isArray(parsed_query[keyword])) {
                api_request[keyword] = parsed_query[keyword].join(",")
            } else {
                api_request[keyword] = parsed_query[keyword]
            }
            // Special case for priority, dirty replace of string by numeric value
            if (keyword === "priority" && api_request["priority"]) {
                for (const prio_name in self.queue.priorityName) {
                    api_request["priority"] = api_request["priority"].replace(prio_name, self.queue.priorityName[prio_name])
                }
            }
        }
    }

    // Set pause action on click of toggle
    self.pauseToggle = function() {
        callAPI({
            mode: (self.downloadsPaused() ? "resume" : "pause")
        }).then(self.refresh);
        self.downloadsPaused(!self.downloadsPaused());
    }

    // Set pause timer
    self.pauseTime = function(item, event) {
        callAPI({
            mode: 'config',
            name: 'set_pause',
            value: $(event.currentTarget).data('time')
        }).then(self.refresh);
        self.downloadsPaused(true);
    };

    // Open modal
    self.openCustomPauseTime = function() {
        // Was it loaded already?
        if (!Date.i18n) {
            jQuery.getScript('./static/javascripts/date.min.js').then(function() {
                // After loading we start again
                self.openCustomPauseTime()
            })
            return;
        }
        // Show modal
        $('#modal-custom-pause').modal('show')
    }

    $('#modal-custom-pause').on('shown.bs.modal', function() {
        // Focus on the input field when opening the modal
        $('#customPauseInput').focus()
    }).on('hide.bs.modal', function() {
        // Reset on modal close
        self.pauseCustom('');
    })

    // Update on changes
    self.pauseCustom.subscribe(function(newValue) {
        // Is it plain numbers?
        if (newValue.match(/^\s*\d+\s*$/)) {
            // Treat it as a number of minutes
            newValue += "minutes";
        }

        // At least 3 charaters
        if (newValue.length < 3) {
            $('#customPauseOutput').text('').data('time', 0)
            $('#modal-custom-pause .btn-default').addClass('disabled')
            return;
        }

        // Fix DateJS bug it has some strange problem with the current day-of-month + 1
        // Removing the space makes DateJS work properly
        newValue = newValue.replace(/\s*h|\s*m|\s*d/g, function(match) {
            return match.trim()
        });

        // Parse
        var pauseParsed = Date.parse(newValue);

        // Did we get it?
        if (pauseParsed) {
            // Is it just now?
            if (pauseParsed <= Date.parse('now')) {
                // Try again with the '+' in front, the parser doesn't get 100min
                pauseParsed = Date.parse('+' + newValue);
            }

            // Calculate difference in minutes and save
            var pauseDuration = Math.round((pauseParsed - Date.parse('now')) / 1000 / 60);
            $('#customPauseOutput').html('<span class="glyphicon glyphicon-pause"></span> ' + glitterTranslate.pauseFor + ' ' + pauseDuration + ' ' + glitterTranslate.minutes)
            $('#customPauseOutput').data('time', pauseDuration)
            $('#modal-custom-pause .btn-default').removeClass('disabled')
        } else if (newValue) {
            // No..
            $('#customPauseOutput').text(glitterTranslate.pausePromptFail)
            $('#modal-custom-pause .btn-default').addClass('disabled')
        }
    })

    // Save custom pause
    self.saveCustomPause = function() {
        // Get duration
        var pauseDuration = $('#customPauseOutput').data('time');

        // If in the future
        if (pauseDuration > 0) {
            callAPI({
                mode: 'config',
                name: 'set_pause',
                value: pauseDuration
            }).then(function() {
                // Refresh and close the modal
                self.refresh()
                self.downloadsPaused(true);
                $('#modal-custom-pause').modal('hide')
            });
        }
    }

    // Update the warnings
    self.nrWarnings.subscribe(function(newValue) {
        // Really any change?
        if (newValue === self.allWarnings().length) return;

        // Get all warnings
        callAPI({
            mode: 'warnings'
        }).then(function(response) {

            // Reset it all
            self.allWarnings.removeAll();
            if (response) {
                // Newest first
                response.warnings.reverse()

                // Go over all warnings and add
                $.each(response.warnings, function(index, warning) {
                    // Reformat CSS label and date
                    // Replaces spaces by non-breakable spaces and newlines with br's
                    var warningData = {
                        index: index,
                        type: glitterTranslate.status[warning.type].slice(0, -1),
                        text: convertHTMLtoText(warning.text).replace(/ /g, '\u00A0').replace(/(?:\r\n|\r|\n)/g, '<br />'),
                        timestamp: warning.time,
                        css: (warning.type === "ERROR" ? "danger" : warning.type === "WARNING" ? "warning" : "info"),
                        clear: self.clearWarnings
                    };
                    self.allWarnings.push(warningData)
                })
            }
        });
    })

    // Clear warnings
    self.clearWarnings = function() {
        callAPI({
            mode: "warnings",
            name: "clear"
        }).done(self.refresh)
    }

    // Clear messages
    self.clearMessages = function(whatToRemove) {
        // Remove specifc type of messages
        self.allMessages.remove(function(item) { return item.index === whatToRemove });
        // Now so we don't show again today
        localStorageSetItem(whatToRemove, Date.now())
    }

    // Update on speed-limit change
    self.speedLimit.subscribe(function(newValue) {
        // Only on new load
        if (!self.speedLimitInt()) return;

        // Update
        if (self.speedLimitInt() !== newValue) {
            callAPI({
                mode: "config",
                name: "speedlimit",
                value: newValue
            })
        }
    });

    // Clear speedlimit
    self.clearSpeedLimit = function() {
        // Send call to override speedlimit
        callAPI({
            mode: "config",
            name: "speedlimit",
            value: 100
        })
        self.speedLimitInt(100.0)
        self.speedLimit(100.0)
    }

    // Shutdown options
    self.setOnQueueFinish = function(model, event) {
        // Something changes
        callAPI({
            mode: 'queue',
            name: 'change_complete_action',
            value: $(event.target).val()
        })
    }

    // Use global settings or device-specific?
    self.useGlobalOptions.subscribe(function(newValue) {
        // Reload in case of enabling global options
        if (newValue) document.location = document.location;
    })

    // Update refreshrate
    self.refreshRate.subscribe(function(newValue) {
        // Refresh now
        self.refresh();

        // Save in config if global-settings
        if (self.useGlobalOptions()) {
            callAPI({
                mode: "set_config",
                section: "misc",
                keyword: "refresh_rate",
                value: newValue
            })
        }
    })

    /***
         Add NZB's
    ***/
    // Updating the label
    self.updateBrowseLabel = function(data, event) {
        // Get filename
        var fileName = $(event.target).val().replace(/\\/g, '/').replace(/.*\//, '');
        // Set label
        if (fileName) $('.btn-file em').text(fileName)
    }

    // Add NZB form
    self.addNZB = function(form) {
        // Anything?
        if (!$(form.nzbFile)[0].files[0] && !$(form.nzbURL).val()) {
            $('.btn-file, input[name="nzbURL"]').attr('style', 'border-color: red !important')
            setTimeout(function() { $('.btn-file, input[name="nzbURL"]').css('border-color', '') }, 2000)
            return false;
        }

        // Disable the buttons to prevent multiple uploads
        let submit_buttons = $(form).find("input[type='submit']")
        submit_buttons.attr("disabled", true)

        // Upload file using the method we also use for drag-and-drop
        if ($(form.nzbFile)[0].files[0]) {
            self.addNZBFromFile($(form.nzbFile)[0].files);
            // Hide modal, upload will reset the form
            $("#modal-add-nzb").modal("hide");
            // Re-enable the buttons
            submit_buttons.attr("disabled", false)
        } else if ($(form.nzbURL).val()) {
            // Or add URL
            var theCall = {
                mode: "addurl",
                name: $(form.nzbURL).val(),
                nzbname: $('#nzbname').val(),
                password: $('#password').val(),
                cat: $('#modal-add-nzb select[name="Category"]').val(),
                priority: $('#modal-add-nzb select[name="Priority"]').val(),
                pp: $('#modal-add-nzb select[name="Processing"]').val(),
                script: $('#modal-add-nzb select[name="Post-processing"]').val(),
            }

            // Add
            callAPI(theCall).then(function(r) {
                // Hide and reset/refresh
                self.refresh()
                $("#modal-add-nzb").modal("hide");
                form.reset()
                $('#nzbname').val('')
                submit_buttons.attr("disabled", false)
            });
        }
    }

    // default to url input when modal is shown
    $('#modal-add-nzb').on('shown.bs.modal', function() {
      $('input[name="nzbURL"]').focus();
    })

    // From the upload or filedrop
    self.addNZBFromFile = function(files, fileindex) {
        // First file
        if (fileindex === undefined) {
            fileindex = 0
        }
        var file = files[fileindex]
        fileindex++

        // Check if it's maybe a folder, we can't handle those
        if (!file.type && file.size % 4096 === 0) return;

        // Add notification
        showNotification('.main-notification-box-uploading', 0, fileindex)

        // Adding a file happens through this special function
        var data = new FormData();
        data.append("name", file);
        data.append("mode", "addfile");
        data.append("nzbname", $('#nzbname').val());
        data.append("password", $('#password').val());
        data.append("cat", $('#modal-add-nzb select[name="Category"]').val())
        data.append("priority", $('#modal-add-nzb select[name="Priority"]').val())
        data.append("pp", $('#modal-add-nzb select[name="Processing"]').val())
        data.append("script", $('#modal-add-nzb select[name="Post-processing"]').val())
        data.append("apikey", apiKey);

        // Add this one
        $.ajax({
            url: "./api",
            type: "POST",
            cache: false,
            processData: false,
            contentType: false,
            data: data
        }).then(function(r) {
            // Are we done?
            if (fileindex < files.length) {
                // Do the next one
                self.addNZBFromFile(files, fileindex)
            } else {
                // Refresh
                self.refresh();
                // Hide notification
                hideNotification()
                // Reset the form
                $('#modal-add-nzb form').trigger('reset');
                $('#nzbname').val('')
                $('.btn-file em').html(glitterTranslate.chooseFile + '&hellip;')
            }
        }).fail(function(xhr, status, error) {
            // Update the uploading notification text to show error
            showNotification('.main-notification-box-uploading-failed', 0, error)
        });
    }

    // Load status info
    self.loadStatusInfo = function(item, event) {
        // Full refresh? Only on click and for the status-screen
        var statusFullRefresh = (event !== undefined) && $('#options-status').hasClass('active');

        // Measure performance? Takes a while
        var statusPerformance = (event !== undefined) && $(event.currentTarget).hasClass('diskspeed-button');

        // Make it spin if the user requested it otherwise we don't,
        // because browsers use a lot of CPU for the animation
        if (statusFullRefresh) {
            self.hasStatusInfo(false)
        }

        // Show loading text for performance measures
        if (statusPerformance) {
            self.hasPerformanceInfo(false)
        }

        // Load the custom status info, allowing for longer timeouts
        callAPI({
            mode: 'status',
            skip_dashboard: (!statusFullRefresh) * 1,
            calculate_performance: statusPerformance * 1,
        }, 30000).then(function(data) {
            // Update basic
            self.statusInfo.folders(data.status.folders)
            self.statusInfo.loadavg(data.status.loadavg)
            self.statusInfo.delayed_assembler(data.status.delayed_assembler)

            // Update the full set if the data is available
            if ("dnslookup" in data.status) {
                self.statusInfo.pystone(data.status.pystone)
                self.statusInfo.downloaddir(data.status.downloaddir)
                self.statusInfo.downloaddirspeed(data.status.downloaddirspeed)
                self.statusInfo.completedir(data.status.completedir)
                self.statusInfo.completedirspeed(data.status.completedirspeed)
                self.statusInfo.internetbandwidth(data.status.internetbandwidth)
                self.statusInfo.dnslookup(data.status.dnslookup)
                self.statusInfo.active_socks5_proxy(data.status.active_socks5_proxy)
                self.statusInfo.localipv4(data.status.localipv4)
                self.statusInfo.publicipv4(data.status.publicipv4)
                self.statusInfo.ipv6(data.status.ipv6 || glitterTranslate.noneText)
            }

            // Update the servers
            ko.mapping.fromJS(data.status.servers, {}, self.statusInfo.servers)

            // Add tooltips to possible new items
            if (!isMobile) $('#modal-options [data-tooltip="true"]').tooltip({ trigger: 'hover', container: 'body' })

            // Stop it spin
            self.hasStatusInfo(true)
            self.hasPerformanceInfo(true)
        });
    }

    // Download a test-NZB
    self.testDownload = function(data, event) {
        var nzbSize = $(event.target).data('size')

        // Maybe it was a click on the icon?
        if (nzbSize === undefined) {
            nzbSize = $(event.target.parentElement).data('size')
        }

        // Build request
        var theCall = {
            mode: "addurl",
            name: "https://sabnzbd.org/tests/test_download_" + nzbSize + ".nzb",
            priority: self.queue.priorityName["Force"]
        }

        // Add
        callAPI(theCall).then(function(r) {
            // Hide and reset/refresh
            self.refresh()
            $("#modal-options").modal("hide");
        });
    }

    // Unblock server
    self.unblockServer = function(servername) {
        callAPI({
            mode: "status",
            name: "unblock_server",
            value: servername
        }).then(function() {
            $("#modal-options").modal("hide");
        })
    }

    // Refresh connections page
    var connectionRefresh
    $('.nav-tabs a[href="#options_connections"]').on('shown.bs.tab', function() {
        // Check size on open
        checkSize()

        // Set the interval
        connectionRefresh = setInterval(function() {
            // Start small
            checkSize()

            // Check if still visible
            if (!$('#options_connections').is(':visible') && connectionRefresh) {
                // Stop refreshing
                clearInterval(connectionRefresh)
                return
            }
            // Update the server stats (speed/connections)
            self.loadStatusInfo()

        }, self.refreshRate() * 1000)
    })

    // On close of the tab
    $('.nav-tabs a[href="#options_connections"]').on('hidden.bs.tab', function() {
        checkSize()
    })

    // Function that handles the actual sizing of connections tab
    function checkSize() {
        // Any connections?
        if (self.showActiveConnections() && $('#options_connections').is(':visible') && $('.table-server-connections').height() > 1) {
            var mainWidth = $('.main-content').width()
            $('#modal-options .modal-dialog').width(mainWidth * 0.85 > 650 ? mainWidth * 0.85 : '')
        } else {
            // Small again
            $('#modal-options .modal-dialog').width('')
        }
    }

    // Make sure Connections get refreshed also after open->close->open
    $('#modal-options').on('show.bs.modal', function() {
        // Trigger
        $('.nav-tabs a[href="#options_connections"]').trigger('shown.bs.tab')
    })

    // Orphaned folder processing
    self.folderProcess = function(folder, htmlElement) {
        // Hide tooltips (otherwise they stay forever..)
        $('#options-orphans [data-tooltip="true"]').tooltip('hide')

        // Show notification on delete
        if ($(htmlElement.currentTarget).data('action') === 'delete_orphan') {
            showNotification('.main-notification-box-removing', 1000)
        } else {
            // Adding back to queue
            showNotification('.main-notification-box-sendback', 2000)
        }

        // Activate
        callAPI({
            mode: "status",
            name: $(htmlElement.currentTarget).data('action'),
            value: $("<div/>").html(folder).text()
        }).then(function() {
            // Refresh
            self.loadStatusInfo(true, true)
            // Hide notification
            hideNotification()
        })
    }

    // Orphaned folder deletion of all
    self.removeAllOrphaned = function() {
        if (confirm(glitterTranslate.clearOrphanWarning)) {
            // Show notification
            showNotification('.main-notification-box-removing-multiple', 0, self.statusInfo.folders().length)
            // Delete them all
            callAPI({
                mode: "status",
                name: "delete_all_orphan"
            }).then(function() {
                // Remove notifcation and update screen
                hideNotification()
                self.loadStatusInfo(true, true)
            })
        }
    }

    // Orphaned folder adding of all
    self.addAllOrphaned = function() {
        if (confirm(glitterTranslate.confirm)) {
            // Show notification
            showNotification('.main-notification-box-sendback')
            // Delete them all
            callAPI({
                mode: "status",
                name: "add_all_orphan"
            }).then(function() {
                // Remove notifcation and update screen
                hideNotification()
                self.loadStatusInfo(true, true)
            })
        }
    }

    // Toggle Glitter's compact layout dynamically
    self.displayCompact.subscribe(function() {
        $('body').toggleClass('container-compact')
    })

    // Toggle full width
    self.displayFullWidth.subscribe(function() {
        $('body').toggleClass('container-full-width')
    })

    // Toggle Glitter's tabbed modus
    self.displayTabbed.subscribe(function() {
        $('body').toggleClass('container-tabbed')
    })

    // Change hash for page-reload
    $('.history-queue-swicher .nav-tabs a').on('shown.bs.tab', function(e) {
        window.location.hash = e.target.hash;
    })

    /**
         SABnzb options
    **/
    // Shutdown
    self.shutdownSAB = function() {
        if (confirm(glitterTranslate.shutdown)) {
            // Show notification and return true to follow the URL
            showNotification('.main-notification-box-shutdown')
            return true
        }
    }
    // Restart
    self.restartSAB = function() {
        if (!confirm(glitterTranslate.restart)) return;
        // Call restart function
        callAPI({ mode: "restart" })

        // Set counter, we need at least 15 seconds
        self.isRestarting(Math.max(1, Math.floor(15 / self.refreshRate())));
        // Force refresh in case of very long refresh-times
        if (self.refreshRate() > 30) {
            setTimeout(self.refresh, 30 * 1000)
        }
    }
    // Queue actions
    self.doQueueAction = function(data, event) {
        // Event
        var theAction = $(event.target).data('mode');
        // Show notification if available
        if (['rss_now', 'watched_now'].indexOf(theAction) > -1) {
            showNotification('.main-notification-box-' + theAction, 2000)
        }
        // Send to the API
        callAPI({ mode: theAction })
    }
    // Repair queue
    self.repairQueue = function() {
        if (!confirm(glitterTranslate.repair)) return;
        // Hide the modal and show the notifucation
        $("#modal-options").modal("hide");
        showNotification('.main-notification-box-queue-repair', 5000)
        // Call the API
        callAPI({ mode: "restart_repair" })
    }
    // Force disconnect
    self.forceDisconnect = function() {
        // Show notification
        showNotification('.main-notification-box-disconnect', 3000)
        // Call API
        callAPI({ mode: "disconnect" }).then(function() {
            $("#modal-options").modal("hide");
        })
    }

    /***
        Retrieve config information and do startup functions
    ***/
    // Force compact mode as fast as possible
    if (localStorageGetItem('displayCompact') === 'true') {
        // Add extra class
        $('body').addClass('container-compact')
    }

    if (localStorageGetItem('displayFullWidth') === 'true') {
        // Add extra class
        $('body').addClass('container-full-width')
    }

    // Tabbed layout?
    if (localStorageGetItem('displayTabbed') === 'true') {
        $('body').addClass('container-tabbed')

        var tab_from_hash = location.hash.replace(/^#/, '');
        if (tab_from_hash) {
            $('.history-queue-swicher .nav-tabs a[href="#' + tab_from_hash + '"]').tab('show');
        }
    }

    self.globalInterfaceSettings = [
        'dateFormat',
        'extraQueueColumns',
        'extraHistoryColumns',
        'displayCompact',
        'displayFullWidth',
        'displayTabbed',
        'confirmDeleteQueue',
        'confirmDeleteHistory',
        'keyboardShortcuts'
    ]

    // Save the rest in config if global-settings
    var saveInterfaceSettings = function(newValue) {
        var interfaceSettings = {}
        for (const setting of self.globalInterfaceSettings) {
            interfaceSettings[setting] = self[setting]
        }
        callAPI({
            mode: "set_config",
            section: "misc",
            keyword: "interface_settings",
            value: ko.toJSON(interfaceSettings)
        })
    }

    // Get the speed-limit, refresh rate and server names
    callAPI({
        mode: 'get_config'
    }).then(function(response) {
        // Do we use global, or local settings?
        if (self.useGlobalOptions()) {
            // Set refreshrate (defaults to 1/s)
            if (!response.config.misc.refresh_rate) response.config.misc.refresh_rate = 1;
            self.refreshRate(response.config.misc.refresh_rate.toString());

            // Set history and queue limit
            self.history.paginationLimit(response.config.misc.history_limit.toString())
            self.queue.paginationLimit(response.config.misc.queue_limit.toString())

            // Import the rest of the settings
            if (response.config.misc.interface_settings) {
                var interfaceSettings = JSON.parse(response.config.misc.interface_settings);
                for (const setting of self.globalInterfaceSettings) {
                    if (setting in interfaceSettings) {
                        self[setting](interfaceSettings[setting]);
                    }
                }
            }
            // Only subscribe now to prevent collisions between localStorage and config settings updates
            for (const setting of self.globalInterfaceSettings) {
                self[setting].subscribe(saveInterfaceSettings);
            }
        }

        // Set bandwidth limit
        if (!response.config.misc.bandwidth_max) response.config.misc.bandwidth_max = false;
        self.bandwithLimit(response.config.misc.bandwidth_max);

        // Reformat and set categories
        self.queue.categoriesList($.map(response.config.categories, function(cat) {
            // Default?
            if(cat.name === '*') return { catValue: '*', catText: glitterTranslate.defaultText };
            return { catValue: cat.name, catText: cat.name };
        }))

        // Get the scripts, if there are any
        if(response.config.misc.script_dir) {
            callAPI({
                mode: 'get_scripts'
            }).then(function(script_response) {
                // Reformat script-list
                self.queue.scriptsList($.map(script_response.scripts, function(script) {
                    // None?
                    if(script === 'None') return { scriptValue: 'None', scriptText: glitterTranslate.noneText };
                    return { scriptValue: script, scriptText: script };
                }))
            })
        }


        // Already set if we are using a proxy
        if (response.config.misc.socks5_proxy_url) self.statusInfo.active_socks5_proxy(true)

        // Set logging and only then subscribe to changes
        self.loglevel(response.config.logging.log_level);
        self.loglevel.subscribe(function(newValue) {
            callAPI({
                mode: "set_config",
                section: "logging",
                keyword: "log_level",
                value: newValue
            });
        })

        // Update message
        if (newRelease) {
            self.allMessages.push({
                index: 'UpdateMsg',
                type: glitterTranslate.status['INFO'],
                text: ('<a class="queue-update-sab" href="' + newReleaseUrl + '" target="_blank">' + glitterTranslate.updateAvailable + ' ' + newRelease + ' <span class="glyphicon glyphicon-save"></span></a>'),
                css: 'info'
            });
        }

        // Message about cache - Not for 5 days if user ignored it
        if (!response.config.misc.cache_limit && localStorageGetItem('CacheMsg') * 1 + (1000 * 3600 * 24 * 5) < Date.now()) {
            self.allMessages.push({
                index: 'CacheMsg',
                type: glitterTranslate.status['INFO'],
                text: ('<a href="./config/general/#cache_limit">' + glitterTranslate.useCache.replace(/<br \/>/g, " ") + ' <span class="glyphicon glyphicon-cog"></span></a>'),
                css: 'info',
                clear: function() { self.clearMessages('CacheMsg') }
            });
        }

        // Message about tips and tricks, only once
        if (response.config.misc.notified_new_skin < 2) {
            self.allMessages.push({
                index: 'TipsMsgV110',
                type: glitterTranslate.status['INFO'],
                text: glitterTranslate.glitterTips + ' <a class="queue-update-sab" href="https://sabnzbd.org/wiki/extra/glitter-tips-and-tricks" target="_blank">Glitter Tips and Tricks <span class="glyphicon glyphicon-new-window"></span></a>',
                css: 'info',
                clear: function() {
                    // Update the config to not show again
                    callAPI({
                        mode: 'set_config',
                        section: 'misc',
                        keyword: 'notified_new_skin',
                        value: 2
                    })

                    // Remove the actual message
                    self.clearMessages('TipsMsgV110')
                }
            });
        }
    })

    // Orphaned folder check - Not for 5 days if user ignored it
    var orphanMsg = localStorageGetItem('OrphanedMsg') * 1 + (1000 * 3600 * 24 * 5) < Date.now();
    // Delay the check
    if (orphanMsg) {
        setTimeout(self.loadStatusInfo, 200);
    }

    // On any status load we check Orphaned folders
    self.hasStatusInfo.subscribe(function(finishedLoading) {
        // Loaded or just starting?
        if (!finishedLoading) return;

        // Orphaned folders? If user clicked away we check again in 5 days
        if (self.statusInfo.folders().length >= 3 && orphanMsg) {
            // Check if not already there
            if (!ko.utils.arrayFirst(self.allMessages(), function(item) { return item.index === 'OrphanedMsg' })) {
                self.allMessages.push({
                    index: 'OrphanedMsg',
                    type: glitterTranslate.status['INFO'],
                    text: glitterTranslate.orphanedJobsMsg + ' <a href="#" onclick="showOrphans()"><span class="glyphicon glyphicon-wrench"></span></a>',
                    css: 'info',
                    clear: function() { self.clearMessages('OrphanedMsg') }
                });
            }
        } else {
            // Remove any message, if it was there
            self.allMessages.remove(function(item) {
                return item.index === 'OrphanedMsg';
            })
        }
    })

    // Message about localStorage not being enabled every 20 days
    if (!hasLocalStorage && localStorageGetItem('LocalStorageMsg') * 1 + (1000 * 3600 * 24 * 20) < Date.now()) {
        self.allMessages.push({
            index: 'LocalStorageMsg',
            type: glitterTranslate.status['WARNING'].replace(':', ''),
            text: glitterTranslate.noLocalStorage,
            css: 'warning',
            clear: function() { self.clearMessages('LocalStorageMsg') }
        });
    }

    if (self.keyboardShortcuts()) {
        $(document).bind('keydown', 'p', function(e) {
            self.pauseToggle();
        });
        $(document).bind('keydown', 'a', function(e) {
            // avoid modal clashes
            if (!$('.modal-dialog').is(':visible')) {
                $('#modal-add-nzb').modal('show');
            }
        });
        $(document).bind('keydown', 'c', function(e) {
            window.location.href = './config/';
        });
        $(document).bind('keydown', 's', function(e) {
            // Update the data
            self.loadStatusInfo(true, true)
            // avoid modal clashes
            if (!$('.modal-dialog').is(':visible')) {
                $('#modal-options').modal('show');
            }
        });
        $(document).bind('keydown', 'shift+left', function(e) {
            if($("body").hasClass("container-tabbed")) {
                $('#history-tab.active > ul.pagination li.active').prev().click();
                $('#queue-tab.active > ul.pagination li.active').prev().click();
            } else {
                $('#history-tab > ul.pagination li.active').prev().click();
                $('#queue-tab > ul.pagination li.active').prev().click();
            }
            e.preventDefault();
        });
        $(document).bind('keydown', 'shift+right', function(e) {
            if($("body").hasClass("container-tabbed")) {
                $('#history-tab.active > ul.pagination li.active').next().click();
                $('#queue-tab.active > ul.pagination li.active').next().click();
            } else {
                $('#history-tab > ul.pagination li.active').next().click();
                $('#queue-tab > ul.pagination li.active').next().click();
            }
            e.preventDefault();
        });
        $(document).bind('keydown', 'shift+up', function(e) {
            if($("body").hasClass("container-tabbed")) {
                $('#history-tab.active > ul.pagination li').first().click();
                $('#queue-tab.active > ul.pagination li').first().click();
            } else {
                $('#history-tab > ul.pagination li').first().click();
                $('#queue-tab > ul.pagination li').first().click();
            }
            e.preventDefault();
        });
        $(document).bind('keydown', 'shift+down', function(e) {
            if($("body").hasClass("container-tabbed")) {
                $('#history-tab.active > ul.pagination li').last().click();
                $('#queue-tab.active > ul.pagination li').last().click();
            } else {
                $('#history-tab > ul.pagination li').last().click();
                $('#queue-tab > ul.pagination li').last().click();
            }
            e.preventDefault();
        });
    }

    /***
        Date-stuff
    ***/
    moment.locale(displayLang);

    // Fill the basic info for date-formats with current date-time
    $('[name="general-date-format"] option').each(function() {
        $(this).text(displayDateTime('', $(this).val()), '')
    })

    // Update the date every minute
    setInterval(function() {
        $('[data-timestamp]').each(function() {
            $(this).text(displayDateTime($(this).data('timestamp'), self.dateFormat(), 'X'))
        })
    }, 60 * 1000)

    /***
        End of main functions, start of the fun!
    ***/
    // Trigger first refresh
    self.interval = setTimeout(self.refresh, parseInt(self.refreshRate()) * 1000);

    // And refresh now!
    self.refresh()

    // Special options for (non) mobile
    if (isMobile) {
        // Disable accept parameter on file inputs, as it doesn't work on mobile Safari
        $("input[accept!=''][accept]").attr("accept","")
    } else {
        // Activate tooltips
        $('[data-tooltip="true"]').tooltip({ trigger: 'hover', container: 'body' })
    }
}

/**
    Model for the whole Queue with all it's items
**/
function QueueListModel(parent) {
    // Internal var's
    var self = this;
    self.parent = parent;
    self.dragging = false;

    // Because SABNZB returns the name
    // But when you want to set Priority you need the number..
    self.priorityName = [];
    self.priorityName["Force"] = 2;
    self.priorityName["High"] = 1;
    self.priorityName["Normal"] = 0;
    self.priorityName["Low"] = -1;
    self.priorityName["Stop"] = -4;
    self.priorityOptions = ko.observableArray([
        { value: 2,  name: glitterTranslate.priority["Force"] },
        { value: 1,  name: glitterTranslate.priority["High"] },
        { value: 0,  name: glitterTranslate.priority["Normal"] },
        { value: -1, name: glitterTranslate.priority["Low"] },
        { value: -4, name: glitterTranslate.priority["Stop"] }
    ]);
    self.processingOptions = ko.observableArray([
        { value: 0, name: glitterTranslate.pp["Download"] },
        { value: 1, name: glitterTranslate.pp["+Repair"] },
        { value: 2, name: glitterTranslate.pp["+Unpack"] },
        { value: 3, name: glitterTranslate.pp["+Delete"] }
    ]);

    // External var's
    self.queueItems = ko.observableArray([]);
    self.totalItems = ko.observable(0);
    self.deleteItems = ko.observableArray([]);
    self.isMultiEditing = ko.observable(false).extend({ persist: 'queueIsMultiEditing' });
    self.isLoading = ko.observable(false).extend({ rateLimit: 100 });
    self.multiEditItems = ko.observableArray([]);
    self.categoriesList = ko.observableArray([]);
    self.scriptsList = ko.observableArray([]);
    self.searchTerm = ko.observable('').extend({ rateLimit: { timeout: 400, method: "notifyWhenChangesStop" } });
    self.paginationLimit = ko.observable(20).extend({ persist: 'queuePaginationLimit' });
    self.pagination = new paginationModel(self);

    // Don't update while dragging
    self.shouldUpdate = function() {
        return !self.dragging;
    }
    self.dragStart = function() {
        self.dragging = true;
    }
    self.dragStop = function(event) {
        // Remove that extra label
        $(event.target).parent().removeClass('table-active-sorting')
        // Wait a little before refreshing again (prevents jumping)
        setTimeout(function() {
            self.dragging = false;
        }, 500)
    }

    // Update slots from API data
    self.updateFromData = function(data) {
        // Get all ID's
        var itemIds = $.map(self.queueItems(), function(i) {
            return i.id;
        });

        // Set limit
        self.totalItems(data.noofslots);

        // Container for new models
        var newItems = [];

        // Go over all items
        $.each(data.slots, function() {
            var item = this;
            var existingItem = ko.utils.arrayFirst(self.queueItems(), function(i) {
                return i.id === item.nzo_id;
            });

            if(existingItem) {
                existingItem.updateFromData(item);
                itemIds.splice(itemIds.indexOf(item.nzo_id), 1);
            } else {
                // Add new item
                newItems.push(new QueueModel(self, item))
            }
        });

        // Remove all items if there's any
        if(itemIds.length === self.paginationLimit()) {
            // Replace it, so only 1 Knockout DOM-update!
            self.queueItems(newItems);
            newItems = [];
        } else {
            // Remove items that don't exist anymore
            $.each(itemIds, function() {
                var id = this.toString();
                self.queueItems.remove(ko.utils.arrayFirst(self.queueItems(), function(i) {
                    return i.id === id;
                }));
            });
        }

        // New items, then add!
        if(newItems.length > 0) {
            ko.utils.arrayPushAll(self.queueItems, newItems);
            self.queueItems.valueHasMutated();
        }

        // Sort every time (takes just few msec)
        self.queueItems.sort(function(a, b) {
            return a.index() < b.index() ? -1 : 1;
        });
    };

    // Move in sortable
    self.move = function(event) {
        var itemMoved = event.item;
        // Up or down?
        var corTerm = event.targetIndex > event.sourceIndex ? -1 : 1;
        // See what the actual index is of the queue-object
        // This way we can see how we move up and down independent of pagination
        var itemReplaced = self.queueItems()[event.targetIndex+corTerm];
        callAPI({
            mode: "switch",
            value: itemMoved.id,
            value2: itemReplaced.index()
        }).then(self.parent.refresh);
    };

    // Move button clicked
    self.moveButton = function(event,ui) {
        var itemMoved = event;
        var targetIndex;
        if($(ui.currentTarget).is(".buttonMoveToTop")){
            //we want to move to the top
            targetIndex = 0;
        } else {
            // we want to move to the bottom
			targetIndex = self.totalItems() - 1;
        }
        callAPI({
            mode: "switch",
            value: itemMoved.id,
            value2: targetIndex
        }).then(self.parent.refresh);

    }

    self.triggerRemoveDownload = function(items) {
        // Show and fill modal
        self.deleteItems.removeAll()

        // Single or multiple items?
        if(items.length) {
            ko.utils.arrayPushAll(self.deleteItems, items)
        } else {
            self.deleteItems.push(items)
        }

        // Show modal or delete right away
        if(self.parent.confirmDeleteQueue()) {
            // Open modal if desired
            $('#modal-delete-queue-job').modal("show")
        } else {
            // Otherwise just submit right away
            $('#modal-delete-queue-job form').submit()
        }
    }

    // Save pagination state
    self.paginationLimit.subscribe(function(newValue) {
        // Save in config if global
        if(self.parent.useGlobalOptions()) {
            callAPI({
                mode: "set_config",
                section: "misc",
                keyword: "queue_limit",
                value: newValue
            })
        }
        // Update pagination and counters
        self.parent.refresh(true)
    });

    // Do we show search box. So it doesn't dissapear when nothing is found
    self.hasQueueSearch = ko.pureComputed(function() {
        return (self.pagination.hasPagination() || self.searchTerm() || (self.parent.hasQueue() && self.isMultiEditing()))
    })

    // Searching in queue (rate-limited in decleration)
    self.searchTerm.subscribe(function() {
        // Go back to page 1
        if(self.pagination.currentPage() !== 1) {
            // This forces a refresh
            self.pagination.moveToPage(1);
        } else {
            // Refresh now
            self.parent.refresh();
        }
    })

    // Clear searchterm
    self.clearSearchTerm = function(data, event) {
        // Was it escape key or click?
        if(event.type === 'mousedown' || (event.keyCode && event.keyCode === 27)) {
            self.isLoading(true)
            self.searchTerm('');
        }
        // Was it click and the field is empty? Then we focus on the field
        if(event.type === 'mousedown' && self.searchTerm() === '') {
            $(event.target).parents('.search-box').find('input[type="text"]').focus()
            return;
        }
        // Need to return true to allow typing
        return true;
    }

    /***
        Multi-edit functions
    ***/
    self.queueSorting = function(data, event) {
        // What action?
        var sort, dir;
        switch($(event.currentTarget).data('action')) {
            case 'sortRemainingAsc':
                sort = 'remaining';
                dir = 'asc';
                break;
            case 'sortAgeAsc':
                sort = 'avg_age';
                dir = 'desc';
                break;
            case 'sortAgeDesc':
                sort = 'avg_age';
                dir = 'asc';
                break;
            case 'sortNameAsc':
                sort = 'name';
                dir = 'asc';
                break;
            case 'sortNameDesc':
                sort = 'name';
                dir = 'desc';
                break;
            case 'sortSizeAsc':
                sort = 'size';
                dir = 'asc';
                break;
            case 'sortSizeDesc':
                sort = 'size';
                dir = 'desc';
                break;
        }

        // Show notification
        showNotification('.main-notification-box-sorting', 2000)

        // Send call
        callAPI({
            mode: 'queue',
            name: 'sort',
            sort: sort,
            dir: dir
        }).then(parent.refresh)
    }

    // Show the input box
    self.showMultiEdit = function() {
        // Update value
        self.isMultiEditing(!self.isMultiEditing())
        // Form
        var $form = $('form.multioperations-selector')

        // Reset form and remove all checked ones
        $form[0].reset();
        self.multiEditItems.removeAll();
        $('.queue-table input[name="multiedit"], #multiedit-checkall-queue').prop({'checked': false, 'indeterminate': false})

        // Is the multi-edit in view?
        if(($form.offset().top + $form.outerHeight(true)) > ($(window).scrollTop()+$(window).height())) {
            // Scroll to form
            $('html, body').animate({
                scrollTop: $form.offset().top + $form.outerHeight(true) - $(window).height() + 'px'
            }, 'fast')
        }
    }

    // Add to the list
    self.addMultiEdit = function(item, event) {
        // Is it a shift-click?
        if(event.shiftKey) {
            checkShiftRange('.queue-table input[name="multiedit"]');
        }

        // Add or remove from the list?
        if(event.currentTarget.checked) {
            // Add item
            self.multiEditItems.push(item);
            // Update them all
            self.doMultiEditUpdate();
        } else {
            // Go over them all to know which one to remove
            self.multiEditItems.remove(function(inList) { return inList.id == item.id; })
        }

        // Update check-all buton state
        setCheckAllState('#multiedit-checkall-queue', '.queue-table input[name="multiedit"]')
        return true;
    }

    // Check all
    self.checkAllJobs = function(item, event) {
        // Get which ones we care about
        var allChecks = $('.queue-table input[name="multiedit"]').filter(':not(:disabled):visible');

        // We need to re-evaltuate the state of this check-all
        // Otherwise the 'inderterminate' will be overwritten by the click event!
        setCheckAllState('#multiedit-checkall-queue', '.queue-table input[name="multiedit"]')

        // Now we can check what happend
        // For when some are checked, or all are checked (but not partly)
        if(event.target.indeterminate || (event.target.checked && !event.target.indeterminate)) {
            var allActive = allChecks.filter(":checked")
            // First remove the from the list
            if(allActive.length == self.multiEditItems().length) {
                // Just remove all
                self.multiEditItems.removeAll();
                // Remove the check
                allActive.prop('checked', false)
            } else {
                // Remove them seperate
                allActive.each(function() {
                    // Go over them all to know which one to remove
                    var item = ko.dataFor(this)
                    self.multiEditItems.remove(function(inList) { return inList.id == item.id; })
                    // Remove the check of this one
                    this.checked = false;
                })
            }
        } else {
            // None are checked, so check and add them all
            allChecks.prop('checked', true)
            allChecks.each(function() { self.multiEditItems.push(ko.dataFor(this)) })
            event.target.checked = true

            // Now we fire the update
            self.doMultiEditUpdate()
        }
        // Set state of all the check-all's
        setCheckAllState('#multiedit-checkall-queue', '.queue-table input[name="multiedit"]')
        return true;
    }

    // Do the actual multi-update immediatly
    self.doMultiEditUpdate = function() {
        // Anything selected?
        if(self.multiEditItems().length < 1) return;

        // Retrieve the current settings
        var newCat = $('.multioperations-selector select[name="Category"]').val()
        var newScript = $('.multioperations-selector select[name="Post-processing"]').val()
        var newPrior = $('.multioperations-selector select[name="Priority"]').val()
        var newProc = $('.multioperations-selector select[name="Processing"]').val()
        var newStatus = $('.multioperations-selector input[name="multiedit-status"]:checked').val()

        // List all the ID's
        var strIDs = '';
        $.each(self.multiEditItems(), function(index) {
            strIDs = strIDs + this.id + ',';
        })

        // All non-category updates need to only happen after a category update
        function nonCatUpdates() {
            if(newScript !== '') {
                callAPI({
                    mode: 'change_script',
                    value: strIDs,
                    value2: newScript
                })
            }
            if(newPrior !== '') {
                callAPI({
                    mode: 'queue',
                    name: 'priority',
                    value: strIDs,
                    value2: newPrior
                })
            }
            if(newProc !== '') {
                callAPI({
                    mode: 'change_opts',
                    value: strIDs,
                    value2: newProc
                })
            }
            if(newStatus) {
                callAPI({
                    mode: 'queue',
                    name: newStatus,
                    value: strIDs
                })
            }

            // Wat a little and do the refresh
            // Only if anything changed!
            if(newStatus || newProc !== '' || newPrior !== '' || newScript !== '' || newCat !== '') {
                setTimeout(parent.refresh, 100)
            }
        }

        // What is changed?
        if(newCat !== '') {
            callAPI({
                mode: 'change_cat',
                value: strIDs,
                value2: newCat
            }).then(nonCatUpdates)
        } else {
            nonCatUpdates()
        }

    }

    // Handle mousedown to capture state before change
    self.handleMultiEditStatusMouseDown = function(item, event) {
        var clickedValue = $(event.currentTarget).find("input").val();

        // If this radio was already selected (same value as previous), clear it
        if ($('.multioperations-selector input[name="multiedit-status"]:checked').val() === clickedValue) {
            // Clear all radio buttons in this group after the click finished
            // Hacky, but it works
            setTimeout(function () {
                $('.multioperations-selector input[name="multiedit-status"]').prop('checked', false);
            }, 200)
        }
        return true;
    }

    // Remove downloads from queue
    self.removeDownloads = function(form) {
        // Hide modal and show notification
        $('#modal-delete-queue-job').modal("hide")
        showNotification('.main-notification-box-removing')

        var strIDs = '';
        $.each(self.deleteItems(), function(index) {
            strIDs = strIDs + this.id + ',';
        })

        callAPI({
            mode: 'queue',
            name: 'delete',
            del_files: 1,
            value: strIDs
        }).then(function(response) {
            self.queueItems.removeAll(self.deleteItems());
            self.multiEditItems.removeAll(self.deleteItems())
            self.parent.refresh();
            hideNotification()
        });
    };

    // Delete all selected
    self.doMultiDelete = function() {
        // Anything selected?
        if(self.multiEditItems().length < 1) return;

        // Trigger modal
        self.triggerRemoveDownload(self.multiEditItems())
    }

    // Move all selected to top
    self.doMultiMoveToTop = function() {
        // Anything selected?
        if(self.multiEditItems().length < 1) return;

        // Move each item to the top, starting from the last one in the sorted list
        var arrayList = self.multiEditItems()
        var movePromises = [];
        for(var i = arrayList.length - 1; i >= 0; i--) {
            movePromises.push(callAPI({
                mode: "switch",
                value: arrayList[i].id,
                value2: 0
            }));
        }

        // Wait for all moves to complete then refresh
        Promise.all(movePromises).then(function() {
            self.parent.refresh();
        });
    }

    // Move all selected to bottom
    self.doMultiMoveToBottom = function() {
        // Anything selected?
        if(self.multiEditItems().length < 1) return;

        // Move each item to the bottom, starting from the first one in the sorted list
        var arrayList = self.multiEditItems()
        var movePromises = [];
        for(var i = 0; i < arrayList.length; i++) {
            movePromises.push(callAPI({
                mode: "switch",
                value: arrayList[i].id,
                value2: self.totalItems() - 1
            }));
        }

        // Wait for all moves to complete then refresh
        Promise.all(movePromises).then(function() {
            self.parent.refresh();
        });
    }

    // Focus on the confirm button
    $('#modal-delete-queue-job').on("shown.bs.modal", function() {
        $('#modal-delete-queue-job .btn[type="submit"]').focus()
    })

    // On change of page we need to check all those that were in the list!
    self.queueItems.subscribe(function() {
        // We need to wait until the unit is actually finished rendering
        setTimeout(function() {
            $.each(self.multiEditItems(), function(index) {
                $('#multiedit_' + this.id).prop('checked', true);
            })

            // Update check-all buton state
            setCheckAllState('#multiedit-checkall-queue', '.queue-table input[name="multiedit"]')
        }, 100)
    }, null, "arrayChange")
}

/**
    Model for each Queue item
**/
function QueueModel(parent, data) {
    var self = this;
    self.parent = parent;
    self.rawLabels = []

    // Job info
    self.id = data.nzo_id;
    self.name = ko.observable($.trim(data.filename));
    self.password = ko.observable(data.password);
    self.index = ko.observable(data.index);
    self.status = ko.observable(data.status);
    self.labels = ko.observableArray(data.labels);
    self.isGrabbing = ko.observable(data.status === 'Grabbing' || data.avg_age === '-')
    self.isFetchingBlocks = data.status === 'Fetching' || data.priority === 'Repair' // No need to update
    self.totalMB = ko.observable(parseFloat(data.mb));
    self.remainingMB = ko.observable(parseFloat(data.mbleft))
    self.missingMB = ko.observable(parseFloat(data.mbmissing))
    self.percentage = ko.observable(parseInt(data.percentage))
    self.avg_age = ko.observable(data.avg_age)
    self.direct_unpack = ko.observable(data.direct_unpack)
    self.category = ko.observable(data.cat);
    self.priority = ko.observable(parent.priorityName[data.priority]);
    self.script = ko.observable(data.script);
    self.unpackopts = ko.observable(parseInt(data.unpackopts)) // UnpackOpts fails if not parseInt'd!
    self.pausedStatus = ko.observable(data.status === 'Paused');
    self.timeLeft = ko.observable(data.timeleft);

    // Initially empty
    self.nameForEdit = ko.observable();
    self.editingName = ko.observable(false);
    self.hasDropdown = ko.observable(false);

    // Color of the progress bar
    self.progressColor = ko.computed(function() {
        // Checking
        if(self.status() === 'Checking') {
            return '#58A9FA'
        }
        // Check for missing data, the value is arbitrary! (2%)
        if(self.missingMB()/self.totalMB() > 0.02) {
            return '#F8A34E'
        }
        // Set to grey, only when not Force download
        if((self.parent.parent.downloadsPaused() && self.priority() !== 2) || self.pausedStatus()) {
            return '#B7B7B7'
        }
        // Nothing
        return '';
    });

    // MB's
    self.progressText = ko.pureComputed(function() {
        if(self.isGrabbing()) {
            return glitterTranslate.fetchingURL
        }
        return (self.totalMB() - self.remainingMB()).toFixed(0) + " MB / " + (self.totalMB() * 1).toFixed(0) + " MB";
    })

    // Texts
    self.name_title = ko.pureComputed(function() {
        // When hovering over the job
        if(self.direct_unpack()) {
            return self.name() + ' - ' + glitterTranslate.status['DirectUnpack'] + ': ' + self.direct_unpack()
        }
        return self.name()
    })
    self.missingText = ko.pureComputed(function() {
        // Check for missing data, can show 0 if article-size is smaller than 500K, but we accept that
        if(self.missingMB()) {
            return self.missingMB().toFixed(0) + ' MB ' + glitterTranslate.misingArt
        }
    })
    self.statusText = ko.computed(function() {
        // Checking
        if(self.status() === 'Checking') {
            return glitterTranslate.checking
        }
        // Grabbing
        if(self.status() === 'Grabbing') {
            return glitterTranslate.fetch
        }
        // Pausing status
        if((self.parent.parent.downloadsPaused() && self.priority() !== 2) || self.pausedStatus()) {
            return glitterTranslate.paused;
        }
        // Just the time
        return rewriteTime(self.timeLeft());
    });

    // Icon to better show force-priority
    self.queueIcon = ko.computed(function() {
        // Force comes first
        if(self.priority() === 2) {
            return 'glyphicon-forward'
        }
        if(self.pausedStatus()) {
            return 'glyphicon-play'
        }
        return 'glyphicon-pause'
    })

    // Extra queue columns
    self.showColumn = function(param) {
        switch(param) {
            case 'category':
                // Exception for *
                if(self.category() === "*")
                    return glitterTranslate.defaultText
                return self.category();
            case 'priority':
                // Onload-exception
                if(self.priority() === undefined) return;
                return ko.utils.arrayFirst(self.parent.priorityOptions(), function(item) { return item.value === self.priority()}).name;
            case 'processing':
                // Onload-exception
                if(self.unpackopts() === undefined) return;
                return ko.utils.arrayFirst(self.parent.processingOptions(), function(item) { return item.value === self.unpackopts()}).name;
            case 'scripts':
                return self.script();
            case 'age':
                return self.avg_age();
        }
        return;
    };

    // Every update
    self.updateFromData = function(data) {
        // Update job info
        self.name($.trim(data.filename));
        self.password(data.password);
        self.index(data.index);
        self.status(data.status)
        self.isGrabbing(data.status === 'Grabbing' || data.avg_age === '-')
        self.totalMB(parseFloat(data.mb));
        self.remainingMB(parseFloat(data.mbleft));
        self.missingMB(parseFloat(data.mbmissing))
        self.percentage(parseInt(data.percentage))
        self.avg_age(data.avg_age)
        self.direct_unpack(data.direct_unpack)
        self.category(data.cat);
        self.priority(parent.priorityName[data.priority]);
        self.script(data.script);
        self.unpackopts(parseInt(data.unpackopts)) // UnpackOpts fails if not parseInt'd!
        self.pausedStatus(data.status === 'Paused');
        self.timeLeft(data.timeleft);

        // Did the label-list change?
        // Otherwise KO will send updates to all texts during refresh()
        if(self.rawLabels !== data.labels.toString()) {
            // Update
            self.labels(data.labels);
            self.rawLabels = data.labels.toString();
        }
    };

    // Pause individual download
    self.pauseToggle = function() {
        callAPI({
            mode: 'queue',
            name: (self.pausedStatus() ? 'resume' : 'pause'),
            value: self.id
        }).then(self.parent.parent.refresh);
    };

    // Edit name
    self.editName = function(data, event) {
        // Not when still grabbing
        if(self.isGrabbing()) return false;

        // Change status and fill
        self.editingName(true)
        self.nameForEdit(self.name())

        // Select the input
        const $input = $(event.target).parents('.name').find('input');
        $input.select();

        // Add Tab/Shift+Tab navigation
        $input.off('keydown.tabnav').on('keydown.tabnav', function (e) {
            if (e.key === 'Tab') {
                e.preventDefault();

                // Find all rename inputs that are currently visible
                const inputs = $('.queue-table input[type="text"]');
                const currentIndex = inputs.index(this);
                let nextIndex = e.shiftKey ? currentIndex - 1 : currentIndex + 1;

                // Wrap around
                if (nextIndex >= inputs.length) nextIndex = 0;
                if (nextIndex < 0) nextIndex = inputs.length - 1;

                // Simulate clicking Rename on the next row
                const $nextRow = inputs.eq(nextIndex).closest('tr');
                $nextRow.find('.hover-button[title="Rename"]').click();

                // Delay focusing to wait for new input to appear
                setTimeout(() => {
                    const $nextInput = $('.queue-table input[type="text"]').eq(nextIndex);
                    $nextInput.focus().select();
                }, 50);
            }
        });
    };


    // Catch the submit action
    self.editingNameSubmit = function() {
        self.editingName(false)
    }

    // Do on change
    self.nameForEdit.subscribe(function(newName) {
        // Anything change or empty?
        if(!newName || self.name() === newName) return;

        // Rename would abort Direct Unpack, so ask if user is sure
        if(self.direct_unpack() && !confirm(glitterTranslate.renameAbort)) return;

        // Send rename
        callAPI({
                mode: 'queue',
                name: 'rename',
                value: self.id,
                value2: newName
            }).then(self.parent.parent.refresh)
    })

    // See items
    self.showFiles = function() {
        // Not when still grabbing
        if(self.isGrabbing()) return false;
        // Trigger update
        parent.parent.filelist.loadFiles(self)
    }

    // Toggle calculation of dropdown
    // Turns out that the <select> in the dropdown are a hugggeeee slowdown on initial load!
    // Only loading on click cuts half the speed (especially on large queues)
    self.toggleDropdown = function(item, event) {
        self.hasDropdown(true)
        // Keep it open!
        keepOpen(event.target)
    }

    // Change of settings
    self.changeCat = function(item, event) {
        callAPI({
            mode: 'change_cat',
            value: item.id,
            value2: item.category()
        }).then(function() {
            // Hide all tooltips before we refresh
            $('.queue-item-settings li').filter('[data-tooltip="true"]').tooltip('hide')
            self.parent.parent.refresh()
        })
    }
    self.changeScript = function(item) {
        callAPI({
            mode: 'change_script',
            value: item.id,
            value2: item.script()
        })
    }
    self.changeProcessing = function(item) {
        callAPI({
            mode: 'change_opts',
            value: item.id,
            value2: item.unpackopts()
        })
    }
    self.changePriority = function(item, event) {
        // Not if we are fetching extra blocks for repair!
        if(item.isFetchingBlocks) return
        callAPI({
            mode: 'queue',
            name: 'priority',
            value: item.id,
            value2: item.priority()
        }).then(function() {
            // Hide all tooltips before we refresh
            $('.queue-item-settings li').filter('[data-tooltip="true"]').tooltip('hide')
            self.parent.parent.refresh()
        })
    }


}

/**
    Model for the whole History with all its items
**/
function HistoryListModel(parent) {
    var self = this;
    self.parent = parent;

    // Variables
    self.lastUpdate = 0;
    self.historyItems = ko.observableArray([])
    self.showFailed = ko.observable(false).extend({ persist: 'historyShowFailed' });
    self.showArchive = ko.observable(false).extend({ persist: 'historyShowArchive' });
    self.permanentlyDelete = ko.observable(false).extend({ persist: 'permanentlyDelete' });
    self.isLoading = ko.observable(false).extend({ rateLimit: 100 });
    self.searchTerm = ko.observable('').extend({ rateLimit: { timeout: 400, method: "notifyWhenChangesStop" } });
    self.paginationLimit = ko.observable(10).extend({ persist: 'historyPaginationLimit' });
    self.totalItems = ko.observable(0);
    self.deleteItems = ko.observableArray([]);
    self.ppItems = ko.observable(0);
    self.pagination = new paginationModel(self);
    self.isMultiEditing = ko.observable(false).extend({ persist: 'historyIsMultiEditing' });
    self.multiEditItems = ko.observableArray([]);

    // Download history info
    self.downloadedToday = ko.observable();
    self.downloadedWeek = ko.observable();
    self.downloadedMonth = ko.observable();
    self.downloadedTotal = ko.observable();

    // Update function for history list
    self.updateFromData = function(data) {
        /***
            See if there's anything to update
        ***/
        if(!data) return;
        self.lastUpdate = data.last_history_update

        /***
            History list functions per item
        ***/
        var itemIds = $.map(self.historyItems(), function(i) {
            return i.historyStatus.nzo_id();
        });

        // For new items
        var newItems = [];
        $.each(data.slots, function(index, slot) {
            var existingItem = ko.utils.arrayFirst(self.historyItems(), function(i) {
                return i.historyStatus.nzo_id() === slot.nzo_id;
            });
            // Set index in the results
            slot.index = index

            // Update or add?
            if(existingItem) {
                existingItem.updateFromData(slot);
                itemIds.splice(itemIds.indexOf(slot.nzo_id), 1);
            } else {
                // Add history item
                newItems.push(new HistoryModel(self, slot));
            }
        });

        // Remove all items
        if(itemIds.length === self.paginationLimit()) {
            // Replace it, so only 1 Knockout DOM-update!
            self.historyItems(newItems);
            newItems = [];
        } else {
            // Remove the un-used ones
            $.each(itemIds, function() {
                var id = this.toString();
                self.historyItems.remove(ko.utils.arrayFirst(self.historyItems(), function(i) {
                    return i.historyStatus.nzo_id() === id;
                }));
            });
        }

        // Add new ones
        if(newItems.length > 0) {
            ko.utils.arrayPushAll(self.historyItems, newItems);
            self.historyItems.valueHasMutated();

            // We also check if it might be in the Multi-edit
            if(self.parent.queue.multiEditItems().length > 0) {
                $.each(newItems, function() {
                    var currentItem = this;
                    self.parent.queue.multiEditItems.remove(function(inList) { return inList.id === currentItem.id; })
                })
            }
        }

        // Sort every time (takes just few msec)
        self.historyItems.sort(function(a, b) {
            return a.index < b.index ? -1 : 1;
        });

        /***
            History information
        ***/
        self.totalItems(data.noofslots);
        self.ppItems(data.ppslots)
        self.downloadedToday(data.day_size);
        self.downloadedWeek(data.week_size);
        self.downloadedMonth(data.month_size);
        self.downloadedTotal(data.total_size);
    };

    // Save pagination state
    self.paginationLimit.subscribe(function(newValue) {
        // Save in config if global config
        if(self.parent.useGlobalOptions()) {
            callAPI({
                mode: "set_config",
                section: "misc",
                keyword: "history_limit",
                value: newValue
            })
        }
        // Update pagination and counters
        self.parent.refresh(true)
    });

    self.triggerRemoveDownload = function(items) {
        // Show and fill modal
        self.deleteItems.removeAll()

        // Single or multiple items?
        if(items.length) {
            ko.utils.arrayPushAll(self.deleteItems, items)
        } else {
            self.deleteItems.push(items)
        }

        // Show modal or delete right away
        if(self.parent.confirmDeleteHistory()) {
            // Open modal if desired
            $('#modal-delete-history-job').modal("show")
        } else {
            // Otherwise just submit right away
            $('#modal-delete-history-job form').submit()
        }
    }

    // Retry a job
    self.retryJob = function(form) {
        // Adding a extra retry file happens through this special function
        var data = new FormData();
        data.append("mode", "retry");
        data.append("nzbfile", $(form.nzbFile)[0].files[0]);
        data.append("value", $('#modal-retry-job input[name="retry_job_id"]').val());
        data.append("password", $('#retry_job_password').val());
        data.append("apikey", apiKey);

        // Add
        $.ajax({
            url: "./api",
            type: "POST",
            cache: false,
            processData: false,
            contentType: false,
            data: data
        }).then(function() {
            self.parent.refresh(true)
        });

        $("#modal-retry-job").modal("hide");
        $('.btn-file em').html(glitterTranslate.chooseFile + '&hellip;')
        form.reset()
    }

    // Searching in history (rate-limited in declaration)
    self.searchTerm.subscribe(function() {
        // Go back to page 1
        if(self.pagination.currentPage() !== 1) {
            // This forces a refresh
            self.pagination.moveToPage(1);
        } else {
            // Make sure we refresh
            self.parent.refresh(true);
        }
    })

    // Clear searchterm
    self.clearSearchTerm = function(data, event) {
        // Was it escape key or click?
        if(event.type === 'mousedown' || (event.keyCode && event.keyCode === 27)) {
            // Set the loader so it doesn't flicker and then switch
            self.isLoading(true)
            self.searchTerm('');
        }
        // Was it click and the field is empty? Then we focus on the field
        if(event.type === 'mousedown' && self.searchTerm() === '') {
            $(event.target).parents('.search-box').find('input[type="text"]').focus()
            return;
        }
        // Need to return true to allow typing
        return true;
    }

    // Toggle showing failed
    self.toggleShowFailed = function(data, event) {
        self.showFailed(!self.showFailed())
        // Force hide tooltip so it doesn't linger
        $('#history-options a').tooltip('hide')
        // Force refresh
        self.parent.refresh(true)
    }

    // Toggle showing archive
    self.toggleShowArchive = function(data, event) {
        self.showArchive(!self.showArchive())
        // Force hide tooltip so it doesn't linger
        $('#history-options a').tooltip('hide')
        // Force refresh
        self.parent.refresh(true)
    }

    // Retry all failed
    self.retryAllFailed = function(data, event) {
        // Ask to be sure
        if(confirm(glitterTranslate.retryAll)) {
            // Send the command
            callAPI({
                mode: 'retry_all'
            }).then(function() {
                // Force refresh
                self.parent.refresh(true)
            })
        }
    }

    // Empty history options
    self.emptyHistory = function(data, event) {
        // What event?
        var whatToRemove = $(event.target).data('action');
        var skipArchive = $('#modal-purge-history input[type="checkbox"]').prop("checked")
        var del_files, value;

        // Purge failed
        if(whatToRemove === 'history-purge-failed') {
            del_files = 0;
            value = 'failed';
        }
        // Also remove files
        if(whatToRemove === 'history-purgeremove-failed') {
            del_files = 1;
            value = 'failed';
        }
        // Remove completed
        if(whatToRemove === 'history-purge-completed') {
            del_files = 0;
            value = 'completed';
        }
        // Remove the ones on this page
        if(whatToRemove === 'history-purge-page') {
            // List all the ID's
            var strIDs = '';
            $.each(self.historyItems(), function(index) {
                // Only append when it's a download that can be deleted
                if(!this.processingDownload() && !this.processingWaiting()) {
                    strIDs = strIDs + this.id + ',';
                }
            })
            // Send the command
            callAPI({
                mode: 'history',
                name: 'delete',
                del_files: 1,
                archive: (!skipArchive) * 1,
                value: strIDs
            }).then(function() {
                // Clear search, refresh and hide
                self.searchTerm('');
                self.parent.refresh();
                $("#modal-purge-history").modal('hide');
            })
            return;
        }

        // Call API and close the window
        callAPI({
            mode: 'history',
            name: 'delete',
            del_files: del_files,
            archive: (!skipArchive) * 1,
            value: value
        }).then(function() {
            self.parent.refresh();
            $("#modal-purge-history").modal('hide');
        });
    };

    // Show the input checkbox
    self.showMultiEdit = function() {
        self.isMultiEditing(!self.isMultiEditing())
        self.multiEditItems.removeAll();
        $('.history-table input[name="multiedit"], #multiedit-checkall-history').prop({'checked': false, 'indeterminate': false})
    }

    // Add to the list
    self.addMultiEdit = function(item, event) {
        // Is it a shift-click?
        if(event.shiftKey) {
            checkShiftRange('.history-table input[name="multiedit"]');
        }

        // Add or remove from the list?
        if(event.currentTarget.checked) {
            // Add item
            self.multiEditItems.push(item);
        } else {
            // Go over them all to know which one to remove
            self.multiEditItems.remove(function(inList) { return inList.id == item.id; })
        }

        // Update check-all buton state
        setCheckAllState('#multiedit-checkall-history', '.history-table input[name="multiedit"]')
        return true;
    }

    // Check all
    self.checkAllJobs = function(item, event) {
        // Get which ones we care about
        var allChecks = $('.history-table input[name="multiedit"]').filter(':not(:disabled):visible');

        // We need to re-evaltuate the state of this check-all
        // Otherwise the 'inderterminate' will be overwritten by the click event!
        setCheckAllState('#multiedit-checkall-history', '.history-table input[name="multiedit"]')

        // Now we can check what happend
        // For when some are checked, or all are checked (but not partly)
        if(event.target.indeterminate || (event.target.checked && !event.target.indeterminate)) {
            var allActive = allChecks.filter(":checked")
            // First remove the from the list
            if(allActive.length == self.multiEditItems().length) {
                // Just remove all
                self.multiEditItems.removeAll();
                // Remove the check
                allActive.prop('checked', false)
            } else {
                // Remove them seperate
                allActive.each(function() {
                    // Go over them all to know which one to remove
                    var item = ko.dataFor(this)
                    self.multiEditItems.remove(function(inList) { return inList.id == item.id; })
                    // Remove the check of this one
                    this.checked = false;
                })
            }
        } else {
            // None are checked, so check and add them all
            allChecks.prop('checked', true)
            allChecks.each(function() { self.multiEditItems.push(ko.dataFor(this)) })
            event.target.checked = true
        }
        // Set state of all the check-all's
        setCheckAllState('#multiedit-checkall-history', '.history-table input[name="multiedit"]')
        return true;
    }

    // Remove downloads from history
    self.removeDownloads = function(form) {
        // Hide modal and show notification
        $('#modal-delete-history-job').modal("hide")
        showNotification('.main-notification-box-removing')

        var strIDsPP = '';
        var strIDsHistory = '';
        $.each(self.deleteItems(), function(index) {
            // Split in jobs that need post-processing aborted, and jobs that need to be deleted
            if(this.processingDownload() === 2) {
                strIDsPP = strIDsPP + this.id + ',';
                // These items should not be listed in the deletedItems later on
                // as active post-processing aren't removed from the history output
                self.deleteItems.remove(this)
            } else {
                strIDsHistory = strIDsHistory + this.id + ',';
            }
        })

        // Trigger post-processing aborting
        if(strIDsPP !== "") {
            callAPI({
                mode: 'cancel_pp',
                value: strIDsPP
            }).then(function(response) {
                // Only hide and refresh
                self.parent.refresh();
                hideNotification()
            });
        }
        if(strIDsHistory !== "") {
            var skipArchive = $('#modal-delete-history-job input[type="checkbox"]').prop("checked")

            // Permanently delete if we are on the Archive page
            if(self.showArchive()) skipArchive = true

            callAPI({
                mode: 'history',
                name: 'delete',
                del_files: 1,
                archive: (!skipArchive) * 1,
                value: strIDsHistory
            }).then(function(response) {
                self.historyItems.removeAll(self.deleteItems());
                self.multiEditItems.removeAll(self.deleteItems())
                self.parent.refresh();
                hideNotification()
            });
        }
    };

    // Delete all selected
    self.doMultiDelete = function() {
        // Anything selected?
        if(self.multiEditItems().length < 1) return;

        // Trigger modal
        self.triggerRemoveDownload(self.multiEditItems())
    }

    // Mark jobs as completed
    self.markAsCompleted = function(items) {
        // Confirm
        if(!confirm(glitterTranslate.markComplete)) {
            return
        }
        // Single or multiple items?
        var strIDs = '';
        if(items.length) {
            $.each(items, function(index) {
                strIDs = strIDs + this.id + ',';
            })
        } else {
            strIDs = items.id
        }

        // Send the API call
        callAPI({
            mode: 'history',
            name: 'mark_as_completed',
            value: strIDs
        }).then(function(response) {
            // Force refresh to update the UI
            self.parent.refresh(true);
        });
    }

    // Mark all selected as completed
    self.doMultiMarkCompleted = function() {
        // Anything selected?
        if(self.multiEditItems().length < 1) return;

        // Mark them
        self.markAsCompleted(self.multiEditItems());
    }

    // Focus on the confirm button
    $('#modal-delete-history-job').on("shown.bs.modal", function() {
        $('#modal-delete-history-job .btn[type="submit"]').focus()
    })

    // On change of page we need to check all those that were in the list!
    self.historyItems.subscribe(function() {
        // We need to wait until the unit is actually finished rendering
        setTimeout(function() {
            $.each(self.multiEditItems(), function(index) {
                $('#multiedit_' + this.id).prop('checked', true);
            })

            // Update check-all buton state
            setCheckAllState('#multiedit-checkall-history', '.history-table input[name="multiedit"]')
        }, 100)
    }, null, "arrayChange")
}

/**
    Model for each History item
**/
function HistoryModel(parent, data) {
    var self = this;
    self.parent = parent;

    // We only update the whole set of information on first add
    // If we update the full set every time it uses lot of CPU
    // The Status/Actionline/scriptline/completed we do update every time
    // When clicked on the more-info button we load the rest again
    self.id = data.nzo_id;
    self.index = data.index;
    self.updateAllHistory = false;
    self.hasDropdown = ko.observable(false);
    self.historyStatus = ko.mapping.fromJS(data);
    self.status = ko.observable(data.status);
    self.action_line = ko.observable(data.action_line);
    self.script_line = ko.observable(data.script_line);
    self.fail_message = ko.observable(data.fail_message);
    self.completed = ko.observable(data.completed);
    self.canRetry = ko.observable(data.retry);

    // Update function
    self.updateFromData = function(data) {
        // Fill all the basic info
        self.index = data.index
        self.status(data.status)
        self.action_line(data.action_line)
        self.script_line(data.script_line)
        self.fail_message(data.fail_message)
        self.completed(data.completed)
        self.canRetry(data.retry)

        // Update all ONCE?
        if(self.updateAllHistory) {
            ko.mapping.fromJS(data, {}, self.historyStatus);
            self.updateAllHistory = false;
        }
    };

    // True/false if failed or not
    self.failed = ko.pureComputed(function() {
        return self.status() === 'Failed';
    });

    // Waiting?
    self.processingWaiting = ko.pureComputed(function() {
        return(self.status() === 'Queued')
    })

    // Processing or done?
    self.processingDownload = ko.pureComputed(function() {
        var status = self.status();
        // When we can cancel
        if (status === 'Extracting' || status === 'Verifying' || status === 'Repairing' || status === 'Running') {
            return 2
        }
        // These cannot be cancelled
        if(status === 'Moving') {
            return 1
        }
        return false;
    })

    // Format status text
    self.statusText = ko.pureComputed(function() {
        if(self.action_line() !== '')
            return self.action_line();
        if(self.status() === 'Failed') // Failed
            return self.fail_message();
        if(self.status() === 'Queued')
            return glitterTranslate.status['Queued'];
        if(self.script_line() === '') // No script line
            return glitterTranslate.status['Completed']

        return self.script_line();
    });

    // Extra history columns
    self.showColumn = function(param) {
        // Picked anything?
        switch(param) {
            case 'speed':
                // Anything to calculate?
                if(self.historyStatus.bytes() > 0 && self.historyStatus.download_time() > 0) {
                    try {
                        // Extract the Download section
                        var downloadLog = ko.utils.arrayFirst(self.historyStatus.stage_log(), function(item) {
                            return item.name() === 'Download'
                        });
                        // Extract the speed
                        return downloadLog.actions()[0].match(/(\S*\s\S+)(?=<br\/>)/)[0]
                    } catch(err) { }
                }
                return;
            case 'category':
                // Exception for *
                if(self.historyStatus.category() === "*")
                    return glitterTranslate.defaultText
                return self.historyStatus.category();
            case 'size':
                return self.historyStatus.size();
        }
        return;
    };

    // Format completion time
    self.completedOn = ko.pureComputed(function() {
        return displayDateTime(self.completed(), parent.parent.dateFormat(), 'X')
    });

    // Format time added
    self.timeAdded = ko.pureComputed(function() {
        return displayDateTime(self.historyStatus.time_added(), parent.parent.dateFormat(), 'X')
    });

    // Subscribe to retryEvent so we can load the password
    self.canRetry.subscribe(function() {
        self.updateAllHistory = true;
    })

    // Re-try button
    self.retry = function() {
        // Set JOB-id
        $('#modal-retry-job input[name="retry_job_id"]').val(self.id)
        // Set password
        $('#retry_job_password').val(self.historyStatus.password())
        // Open modal
        $('#modal-retry-job').modal("show")
    };

    // Mark as completed button
    self.markAsCompleted = function() {
        parent.markAsCompleted(self);
    };

    // Update information only on click
    self.updateAllHistoryInfo = function(data, event) {
        // Show
        self.hasDropdown(true);

        // Update all info
        self.updateAllHistory = true;
        parent.parent.refresh(true);

        // Try to keep open
        keepOpen(event.target)
    }

    // Use KO-afterRender to add the click-functionality always
    self.addHistoryStatusStuff = function(item) {
        $(item).find('.history-status-modallink a').click(function(e) {
            // Modal or 'More' click?
            if($(this).is('.history-status-dmca')) {
                // Pass
                return true;
            } else if($(this).is('.history-status-more')) {
                // Expand the rest of the text and hide the button
                $(this).siblings('.history-status-hidden').slideDown()
                $(this).hide()
            } else {
               // Info in modal
                $('#history-script-log .modal-body').load($(this).attr('href'), function(result) {
                    // Set title and then remove it
                    $('#history-script-log .modal-title').text($(this).find("h3").text())
                    $(this).find("h3, title").remove()
                    $('#history-script-log').modal('show');
                });
            }
            return false;
        })
    }
}

// For the file-list
function Fileslisting(parent) {
    var self = this;
    self.parent = parent;
    self.fileItems = ko.observableArray([]);

    // Prevent flash of unstyled content when deleting items
    self.fileItems.extend({ rateLimit: 50 })

    // Need to reserve these names to be overwritten
    self.filelist_name = ko.observable();
    self.filelist_password = ko.observable();

    // Load the function and reset everything
    self.loadFiles = function(queue_item) {
        // Update
        self.currentItem = queue_item;
        self.fileItems.removeAll()
        self.triggerUpdate()

        // Update name/password
        self.filelist_name(self.currentItem.name())
        self.filelist_password(self.currentItem.password())

        // Hide ok button and reset
        $('#modal-item-filelist .glyphicon-floppy-saved').hide()
        $('#modal-item-filelist .glyphicon-lock').show()

        // Set state of the check-all
        setCheckAllState('#modal-item-files .multioperations-selector input[type="checkbox"]', '#modal-item-files .files-sortable input')

        // Show
        $('#modal-item-files').modal('show');

        // Stop updating on closing of the modal
        $('#modal-item-files').on('hidden.bs.modal', function () {
            self.removeUpdate();
        })
    }

    // Trigger update
    self.triggerUpdate = function() {
        // Call API
        callAPI({
            mode: 'get_files',
            value: self.currentItem.id
        }).then(function(response) {
            // When there's no files left we close the modal and the update will be stopped
            // For example when the job has finished downloading
            if(response.files.length === 0) {
                $('#modal-item-files').modal('hide');
                return;
            }

            // Go over them all
            var newItems = [];
            $.each(response.files, function(index, slot) {
                // Existing or updating?
                var existingItem = ko.utils.arrayFirst(self.fileItems(), function(i) {
                    return i.nzf_id() === slot.nzf_id;
                });

                if(existingItem) {
                    // Update the rest
                    existingItem.updateFromData(slot);
                } else {
                    // Add files item
                    newItems.push(new FileslistingModel(self, slot));
                }
            })

            // Add new ones in 1 time instead of every single push
            if(newItems.length > 0) {
                ko.utils.arrayPushAll(self.fileItems, newItems);
                self.fileItems.valueHasMutated();
            }

            // Check if we show/hide completed
            if(localStorageGetItem('showCompletedFiles') === 'No') {
                $('.item-files-table tr.files-done').hide();
                $('#filelist-showcompleted').removeClass('hover-button')
            }

            // Refresh with same as rest
            self.setUpdate()
        })
    }

    // Set update
    self.setUpdate = function() {
        self.updateTimeout = setTimeout(function() {
            self.triggerUpdate()
        }, parent.refreshRate() * 1000)
    }

    // Remove the update
    self.removeUpdate = function() {
        clearTimeout(self.updateTimeout)
    }

    // Move in sortable
    self.move = function(event) {
        // How much did we move?
        var nrMoves = event.sourceIndex - event.targetIndex;
        var direction = (nrMoves > 0 ? 'up' : 'down')

        callAPI({
            mode: 'move_nzf_bulk',
            name: direction,
            value: self.currentItem.id,
            nzf_ids: event.item.nzf_id(),
            size: Math.abs(nrMoves)
        }).then(function() {
            // Refresh all the files
            self.loadFiles(self.currentItem)
        })
    };

    // Move to top and bottom buttons
    self.moveButton = function (item, event) {
        // Up or down?
        var direction = "bottom"
        if ($(event.currentTarget).is(".buttonMoveToTop")) {
            // we are moving to the top
            direction = "top"
        }

        callAPI({
            mode: 'move_nzf_bulk',
            name: direction,
            value: self.currentItem.id,
            nzf_ids: item.nzf_id()
        }).then(function() {
            // Refresh all the files
            self.loadFiles(self.currentItem)
        })
    };

    // Remove selected files
    self.removeSelectedFiles = function() {
        // Get all selected ones
        var nzfIds = []
        $('.item-files-table input:checked:not(:disabled)').each(function() {
            // Add this item
            nzfIds.push($(this).prop('name'))
        })

        callAPI({
            mode: 'queue',
            name: 'delete_nzf',
            value: self.currentItem.id,
            value2: nzfIds.join()
        }).then(function() {
            // Refresh all the files
            self.loadFiles(self.currentItem)
        })
    }

    // For changing the passwords
    self.setNzbPassword = function() {
        // Have to also send the current name for it to work
        callAPI({
                mode: 'queue',
                name: 'rename',
                value: self.currentItem.id,
                value2: self.currentItem.name(),
                value3: $('#nzb_password').val()
        }).then(function() {
            // Refresh, reset and close
            parent.refresh()
            $('#modal-item-filelist .glyphicon-floppy-saved').show()
            $('#modal-item-filelist .glyphicon-lock').hide()
            $('#modal-item-files').modal('hide')
        })
        return false;
    }

    // Check all
    self.checkAllFiles = function(item, event) {
        // Get which ones we care about
        var allChecks = $('#modal-item-files .files-sortable input').filter(':not(:disabled):visible');

        // We need to re-evaltuate the state of this check-all
        // Otherwise the 'inderterminate' will be overwritten by the click event!
        setCheckAllState('#modal-item-files .multioperations-selector input[type="checkbox"]', '#modal-item-files .files-sortable input')

        // Now we can check what happend
        if(event.target.indeterminate) {
            allChecks.filter(":checked").prop('checked', false)
        } else {
            // Toggle their state by a click
            allChecks.prop('checked', !event.target.checked)
            event.target.checked = !event.target.checked;
            event.target.indeterminate = false;
        }
        // Set state of all the check-all's
        setCheckAllState('#modal-item-files .multioperations-selector input[type="checkbox"]', '#modal-item-files .files-sortable input')
        return true;
    }

    // For selecting range and the check-all button
    self.checkSelectRange = function(data, event) {
        if(event.shiftKey) {
            checkShiftRange('#modal-item-files .files-sortable input:not(:disabled)')
        }
        // Set state of the check-all
        setCheckAllState('#modal-item-files .multioperations-selector input[type="checkbox"]', '#modal-item-files .files-sortable input')
        return true;
    }
}

// Indiviual file models
function FileslistingModel(parent, data) {
    var self = this;
    // Define veriables
    self.filename = ko.observable(data.filename);
    self.nzf_id = ko.observable(data.nzf_id);
    self.file_age = ko.observable(data.age);
    self.mb = ko.observable(data.mb);
    self.canselect = ko.observable(data.status !== "finished" && data.status !== "queued");
    self.isdone =  ko.observable(data.status === "finished");
    self.percentage = ko.observable(self.isdone() ? fixPercentages(100) : fixPercentages((100 - (data.mbleft / data.mb * 100)).toFixed(0)));

    // Update internally
    self.updateFromData = function(data) {
        self.filename(data.filename)
        self.nzf_id(data.nzf_id)
        self.file_age(data.age)
        self.mb(data.mb)
        self.canselect(data.status !== "finished" && data.status !== "queued")
        self.isdone(data.status === "finished")
        // Data is given in MB, would always show 0% for small files even if completed
        self.percentage(self.isdone() ? fixPercentages(100) : fixPercentages((100 - (data.mbleft / data.mb * 100)).toFixed(0)))
    }
}

// Model for pagination, since we use it multiple times
function paginationModel(parent) {
    var self = this;

    // Var's
    self.nrPages = ko.observable(0);
    self.currentPage = ko.observable(1);
    self.currentStart = ko.observable(0);
    self.allpages = ko.observableArray([]).extend({ rateLimit: 50 });

    // Has pagination
    self.hasPagination = ko.pureComputed(function() {
        return self.nrPages() > 1;
    })

    // Subscribe to number of items
    parent.totalItems.subscribe(function() {
        // Update
        self.updatePages();
    })

    // Subscribe to changes of pagination limit
    parent.paginationLimit.subscribe(function(newValue) {
        self.updatePages();
        self.moveToPage(self.currentPage());
    })

    // Easy handler for adding a page-link
    self.addPaginationPageLink = function(pageNr) {
        // Return object for adding
        return {
            page: pageNr,
            isCurrent: pageNr === self.currentPage(),
            isDots: false,
            onclick: function(data) {
                self.moveToPage(data.page);
            }
        }
    }

    // Easy handler to add dots
    self.addDots = function() {
        return {
            page: '...',
            isCurrent: false,
            isDots: true,
            onclick: function() {}
        }
    }

    self.updatePages = function() {
        // Empty it
        self.allpages.removeAll();

        // How many pages do we need?
        if(parent.totalItems() <= parent.paginationLimit()) {
            // Empty it
            self.nrPages(1)
            self.currentStart(0);

            // Are we on next page? Bad!
            if(self.currentPage() > 1) {
                // Force full update
                parent.parent.refresh(true);
            }

            // Move to current page
            self.currentPage(1);
        } else {
            // Calculate number of pages needed
            var newNrPages = Math.ceil(parent.totalItems() / parent.paginationLimit())

            // Make sure the current page still exists
            if(self.currentPage() > newNrPages) {
                self.moveToPage(newNrPages);
                return;
            }

            // All the cases
            if(newNrPages > 7) {
                // Do we show the first ones
                if(self.currentPage() < 5) {
                    // Just add the first 4
                    $.each(new Array(5), function(index) {
                        self.allpages.push(self.addPaginationPageLink(index + 1))
                    })
                    // Dots
                    self.allpages.push(self.addDots())
                    // Last one
                    self.allpages.push(self.addPaginationPageLink(newNrPages))
                } else {
                    // Always add the first
                    self.allpages.push(self.addPaginationPageLink(1))
                        // Dots
                    self.allpages.push(self.addDots())

                    // Are we near the end?
                    if((newNrPages - self.currentPage()) < 4) {
                        // We add the last ones
                        $.each(new Array(5), function(index) {
                            self.allpages.push(self.addPaginationPageLink((index - 4) + (newNrPages)))
                        })
                    } else {
                        // We are in the center so display the center 3
                        $.each(new Array(3), function(index) {
                            self.allpages.push(self.addPaginationPageLink(self.currentPage() + (index - 1)))
                        })

                        // Dots
                        self.allpages.push(self.addDots())
                            // Last one
                        self.allpages.push(self.addPaginationPageLink(newNrPages))
                    }
                }
            } else {
                // Just add them
                $.each(new Array(newNrPages), function(index) {
                    self.allpages.push(self.addPaginationPageLink(index + 1))
                })
            }

            // Change of number of pages?
            if(newNrPages !== self.nrPages()) {
                // Update
                self.nrPages(newNrPages);
            }
        }
    }

    // Update on click
    self.moveToPage = function(page) {
        // Update page and start
        self.currentPage(page)
        self.currentStart((page - 1) * parent.paginationLimit())
        // Re-paginate
        self.updatePages();
        // Force full update
        parent.parent.refresh(true);
    }
}

    // GO!!!
    ko.applyBindings(new ViewModel(), document.getElementById("sabnzbd"));
});
