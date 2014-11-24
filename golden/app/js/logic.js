'use strict';


/* Add this to the skia namespace */
var skia = skia || {};
(function (ns) {
  // c contains all constants. Primarily relating to backend resources.
  // They need to match the definitions in go/skiacorrectness/main.go
  ns.c = {
    // PREFIX_URL is the prefix to all backend request to JSON resources.
    PREFIX_URL: '/rest',

    // URLs exposed by the backend to retrieve JSON data.
    URL_COUNTS: '/counts',
    URL_TRIAGE: '/triage',

    URL_LOGIN_STATUS: '/loginstatus',
    URL_LOGOUT: '/logout',

    // The triage labels need to match the values in golden/go/types/types.go
    UNTRIAGED: 0,
    POSITIVE: 1,
    NEGATIVE: 2,

    // Key in parameters that identifies a test.
    PRIMARY_KEY_FIELD: 'name',

    // Param fields to filter.
    PARAMS_FILTER: {
      'name': true,
      'source_type': true,
    }
  };

    /**
     * Plot is a class that wraps the flot object and exposes draw functions.
     *
     * @param {jQueryElement} element This a jquery element to which the
                                      flot instance to attach to.
     *
     * @return {Plot} instance of Plot class.
     **/

  ns.Plot = function (element) {
    this.element = element;

    // initialize the flot element with empty data.
    this.flotObj = element.plot([], {
      legend: {
        show: true
      },
      xaxis: {
        show: true
      }
    }).data('plot');
  };


  /**
   * setData sets the data that the plot needs to draw and forces a redraw.
   * If ticks is not null it will also set the ticks and reset the x-axis.
   *
     * @param {array} data Array of series understood by Flot. See
     *           https://github.com/flot/flot/blob/master/API.md#data-format
     *
     * @param {array} ticks Array or function that defines the ticks for the
     *                      x-axis.
  */
  ns.Plot.prototype.setData = function(data, ticks) {
    this.flotObj.setData(data);

    // Set the ticks on the x axis if necessary.
    if (ticks) {
      var opt = this.flotObj.getOptions();
      opt.xaxes.forEach(function(axis) {
        axis.ticks = ticks;
      });
    }

    // redraw the graph
    this.flotObj.setupGrid();
    this.flotObj.draw();
  };

  /**
   * PlotData is a class that used as the return value of processAllCounts and
   * contains the processed data.
   *  */
  ns.PlotData = function (data, ticks, allAggregates, testDetails) {
    this.plotData = data;
    this.ticks = ticks;
    this.testDetails = testDetails;
    this.allAggregates = allAggregates;
  };

  /**
   * getTicks returns the ticks for the PlotData object at hand.
   */
  ns.PlotData.prototype.getTicks = function (axis) {
    return this.ticks;
  };

  /**
  * TestDetails is a class that contails the aggregated information about
  * a single tests. It is derived from the data returned by the server.
  */
  ns.TestDetails = function (name, counts) {
    this.name = name;
    this.counts = counts;
    this.aggregates = aggregateCounts(counts);
  };

  /**
  * DiffDigestInfo is a helper class to store information about a
  * digest (usually positive) and how it differs from the a given
  * untriaged digest.
  */
  ns.DiffDigestInfo = function (digest, imgUrl, count, diff) {
    this.digest = digest;
    this.imgUrl = imgUrl;
    this.counts = count;
    this.diff = diff;
  };

  /**
  * isIdentical returns true if the current digest is identical
  *             to the untriaged digest.
  */
  ns.DiffDigestInfo.prototype.isIdentical = function () {
    return this.diff.numDiffPixels === 0;
  }

  /**
  * addIndexAsX adds takes an array of numbers and returns an array of
  * datapoints (x,y) where x is the index of the input element y.
  */
  function addIndexAsX(arr) {
    var result = [];
    for(var i=0, len=arr.length; i<len; i++) {
      result.push([i, arr[i]])
    }
    return result;
  }

  /**
  * aggregateCounts sums over the counts contained in an object.
  * Each member in the object is assumed to be an array of numbers.
  *
  * @param { object } countsObj contains attributes where each attribute
  *                   is an array of numbers.
  * @return {object} an array with the same attributes as the input object.
  *                  Each attribute contains the sum of the corresponding
  *                  array.
  */
  function aggregateCounts(countsObj) {
    var result = {};
    var arr;
    for(var k in countsObj) {
      if (countsObj.hasOwnProperty(k)) {
        result[k] = 0;
        arr = countsObj[k];
        for(var i=0,len=arr.length; i < len; i++) {
          result[k] += arr[i];
        }
      }
    }
    return result;
  }

  /**
  * processAllCounts converts the data returned by the server to
  *  an instance of PlotData that can then be used to render the UI
  *  and also serve as input to the Plot class.
  *
  * @param {object} serverData returned from the server containing the
  *                 aggregated values over all tests.
  *
  * @param {string} testName specifies whether we want to get the data
  *                 for a specific test. If null all data are returned.
  *
  * @return {object} instance of PlotData.
  *
  */
  ns.processCounts = function (serverData, testName) {
    // get the counts from the tests.
    var testCounts = [];
    if (testName && serverData.counts.hasOwnProperty(testName)) {
      testCounts.push(new ns.TestDetails(testName, serverData.counts[testName]))
    }
    else {
      for(var tName in serverData.counts) {
        if (serverData.counts.hasOwnProperty(tName)) {
          testCounts.push(new ns.TestDetails(tName, serverData.counts[tName]));
        }
      }
    }

    // assemble the plot data.
    var targetData = testName ? serverData.counts[testName] : serverData.aggregated;
    var data = [];
    for(var k in targetData) {
      if (targetData.hasOwnProperty(k)) {
        data.push({
          label: k,
          lines: {
            show: true,
            steps: true
          },
          data: addIndexAsX(targetData[k])
        });
      }
    }

    return new ns.PlotData(data,
                           serverData.ticks,
                           aggregateCounts(serverData.aggregated),
                           testCounts);
  };

  /**
  *  getUntriagedSorted returns the untriaged digests sorted by largest
  *  deviation from a positively labeled digest. It processes the data
  *  directly returned by the backend.
  *  It also resolves the references to the positive digests contained in
  *  the diff metrics.
  */
  ns.getUntriagedSorted = function(serverData, testName) {
    var unt = robust_get(serverData, ['tests', testName, 'untriaged']);
    if (!unt) {
      return [];
    }

    var posd, d;
    var result = [];
    var positive = serverData.tests[testName].positive;
    var hasPos = false;

    for (var digest in unt) {
      if (unt.hasOwnProperty(digest)) {
        var posDiffs = [];
        for(var i=0, len=unt[digest].diffs.length; i < len; i++) {
          // TODO (stephana): Fill in expanding the diff information.
          // This will be done once triaging works. So we can test it
          // with real data.
          hasPos = true;
          d = unt[digest].diffs[i];
          posd = positive[d.posDigest];
          posDiffs.push(new ns.DiffDigestInfo(d.posDigest, posd.imgUrl, posd.count, d));
        }

        // Inject the digest and the augmented positive diffs.
        unt[digest].digest = digest;
        unt[digest].positiveDiffs = posDiffs;
        result.push(unt[digest]);
      }
    }

    // Sort the result increasing by pixel difference or
    // decreasing by counts if there are no positives.
    var sortFn;
    if (hasPos) {
      sortFn = function (a,b) {
        return a.positiveDiffs[0].diff.numDiffPixels - b.positiveDiffs[0].diff.numDiffPixels;
      };
    } else {
      sortFn = function (a,b) { return b.count - a.count; };
    }
    result.sort(sortFn);

    return result;
  };

  /**
  * getSortedPositivesFromUntriaged returns the list of positively labeled
  * digests. It assumes that 'untriagedRec' was generated by a call to
  * getUntriagedSorted(...).
  */
  ns.getSortedPositivesFromUntriaged = function (untriagedRec) {
    if (untriagedRec && untriagedRec.positiveDiffs && untriagedRec.positiveDiffs.length > 0) {
      return untriagedRec.positiveDiffs;
    }

    return [];
  };

  /**
  * getSortedPositives returns a list of positive digests from the
  * data returnded by the backend. This is to be used when there are no
  * untriaged digests.
  */
  ns.getSortedPositives = function (serverData, testName) {
    var pos = robust_get(serverData, ['tests', testName, 'positive']);
    if (!pos) {
      return [];
    }

    var result = [];
    for (var digest in pos) {
      if (pos.hasOwnProperty(digest)) {
        // Inject the digest into the object.
        pos[digest].digest = digest;
        result.push(pos[digest]);
      }
    }

    // TODO: sort the result.

    return result;
  };

  /**
  * getSortedParams returns all parameters and the union of their values as a
  * (nested) sorted Array in the format:
  *       [[param1, [val1, val2, ...],
           [param2, [val3, val4, ...], ... ]]]
  */
  ns.getSortedParams = function (serverData, filter) {
    var result = [];
    for(var k in serverData.allParams) {
      if (serverData.allParams.hasOwnProperty(k) && (!filter || !ns.c.PARAMS_FILTER[k])) {
        serverData.allParams[k].sort();
        result.push([k, serverData.allParams[k]]);
      }
    }

    result.sort(function(a,b){
      return (a[0] < b[0]) ? -1 : (a[0] > b[0]) ? 1 : 0;
    });

    return result;
  };

  /**
  * getNumArray returns an array of numbers of the given length and each
  * element is initialized with initVal. If initVal is omitted, 0 (zero)
  * is used instead.
  */
  ns.getNumArray = function(len, initVal) {
    if (!initVal) {
      initVal = 0;
    }

    var result = [];
    for(var i =0; i < len; i++) {
      result.push(initVal);
    }

    return result;
  };

  /**
  * TriageDigestReq is a container type for sending labeled digests to the
  * backend. It matches the input parameters of the triageDigestsHandler in
  * 'go/skiacorrectness/main.go'.
  */

  ns.TriageDigestReq = function () {
  };

  /**
  * addDigestLabel is a convenience method to add digests and their label to the
  * the instance.
  */
  ns.TriageDigestReq.prototype.add = function (testName, digest, label) {
    this[testName] = this[testName] || {};
    this[testName][digest] = label;
  };

  /////////////////////////////////////////////////////////////////
  // Generic utility functions.
  /////////////////////////////////////////////////////////////////
  /*
  * isEmpty returns true if the provided object is empty and false
  *         otherwise.
  */
  ns.isEmpty = function (obj) {
    for (var k in obj) {
      if (obj.hasOwnProperty(k)) {
        return false;
      }
    }
    return true;
  };

  /*
  */
  ns.extractQueryString = function (url) {
    var idx = url.indexOf('?');
    return (idx === -1) ? '' : url.substring(idx);
  };

  /////////////////////////////////////////////////////////////////
  // Utility functions that are not exposed in the namespace.
  /////////////////////////////////////////////////////////////////

  /**
   * robust_get finds a sub object within 'obj' by following the path
   * in 'idx'. It will not throw an error if any sub object is missing
   * but instead return 'undefined'.
   **/
  function robust_get(obj, idx) {
    if (!idx) {
      return;
    }

    for(var i=0, len=idx.length; i<len; i++) {
      if ((typeof obj === 'undefined') || (!idx[i])) {
        return;  // returns 'undefined'
      }

      obj = obj[idx[i]];
    }

    return obj;
  }


})(skia);
